// Package monitor brings together what borgmatic reports and what the
// repositories contain, and judges it. It never starts, changes or deletes
// backups.
package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/borg"
	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

type Monitor struct {
	Cfg     *config.Config
	Store   *store.Store
	Backend borg.Backend
	Log     *slog.Logger
	Demo    bool
	Now     func() time.Time
	// Notifier: messages on status changes (nil = off).
	Notifier Notifier

	mu       sync.Mutex
	inflight map[string]bool
}

func New(cfg *config.Config, st *store.Store, b borg.Backend, log *slog.Logger) *Monitor {
	return &Monitor{Cfg: cfg, Store: st, Backend: b, Log: log, Now: time.Now, inflight: map[string]bool{}}
}

func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Run refreshes the repositories in the background until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	go m.Backend.Detect(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	m.refreshDue(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.refreshDue(ctx)
			m.checksDue()
			m.notifyChanges(ctx)
		}
	}
}

func (m *Monitor) refreshDue(ctx context.Context) {
	now := m.Now()
	for _, j := range m.Cfg.Jobs() {
		for i := range j.Job.Repositories {
			repo := &j.Job.Repositories[i]
			if !repo.Queried() {
				continue
			}
			var snap *store.RepoSnapshot
			m.Store.Read(func(s *store.State) { snap = s.Repos[repo.ID] })
			if snap != nil && now.Sub(snap.CheckedAt) < m.Cfg.Borg.RefreshInterval.D() {
				continue
			}
			go func() { _, _ = m.refresh(ctx, repo) }()
		}
	}
}

var ErrBusy = errors.New("Abfrage läuft bereits")

// refresh lists one repository (and its sizes when due) and stores the result.
func (m *Monitor) refresh(ctx context.Context, repo *config.Repository) (*store.RepoSnapshot, error) {
	m.mu.Lock()
	if m.inflight[repo.ID] {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	m.inflight[repo.ID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.inflight, repo.ID)
		m.mu.Unlock()
	}()

	snap, berr := m.Backend.List(ctx, repo)
	var old *store.RepoSnapshot
	m.Store.Read(func(s *store.State) {
		if o := s.Repos[repo.ID]; o != nil {
			c := *o
			old = &c
		}
	})
	if berr != nil {
		m.Log.Warn("repository query failed", "repo", repo.ID, "summary", berr.Finding.Summary, "code", berr.Code)
		snap.OK = false
		f := berr.Finding
		snap.Error = &f
		if old != nil { // keep the last known archives – shown as such, with their date
			snap.LastGoodAt, snap.ArchiveCount, snap.Latest, snap.Recent = old.LastGoodAt, old.ArchiveCount, old.Latest, old.Recent
			snap.Encryption, snap.Sizes, snap.LastModified, snap.Coverage = old.Encryption, old.Sizes, old.LastModified, old.Coverage
		}
	} else {
		t := snap.CheckedAt
		snap.LastGoodAt = &t
		if old != nil {
			snap.Sizes, snap.SizesError = old.Sizes, old.SizesError
		}
		if ref, _, ok := m.Cfg.RepoByID(repo.ID); ok {
			m.checkCoverage(ctx, ref.Job, repo, snap, old)
		}
		if iv := m.Cfg.Borg.SizesInterval.D(); iv > 0 && (snap.Sizes == nil || m.Now().Sub(snap.Sizes.At) > iv) {
			if sz, serr := m.Backend.Sizes(ctx, repo); serr != nil {
				snap.SizesError = serr.Finding.Summary
			} else {
				snap.Sizes, snap.SizesError = sz, ""
			}
		}
	}
	err := m.Store.Update(func(s *store.State) { s.Repos[repo.ID] = snap })
	return snap, err
}

// RefreshNow is the manual "Jetzt aktualisieren" – with a cooldown so a
// button cannot hammer a repository.
func (m *Monitor) RefreshNow(ctx context.Context, repoID string) error {
	_, repo, ok := m.Cfg.RepoByID(repoID)
	if !ok {
		return errors.New("unbekanntes Repository")
	}
	if !repo.Queried() {
		return errors.New("dieses Repository wird nicht abgefragt (query: false)")
	}
	var snap *store.RepoSnapshot
	m.Store.Read(func(s *store.State) { snap = s.Repos[repoID] })
	if snap != nil && m.Now().Sub(snap.CheckedAt) < m.Cfg.Borg.ManualCooldown.D() {
		return errors.New("gerade erst abgefragt – bitte kurz warten")
	}
	_, err := m.refresh(ctx, repo)
	return err
}

// --- reports from borgmatic -------------------------------------------------

type PingKind string

const (
	PingStart   PingKind = "start"
	PingSuccess PingKind = "success"
	PingFail    PingKind = "fail"
	PingLog     PingKind = "log"
	PingExit    PingKind = "exit" // with exit code
)

// Ping records a report: borgmatic's Healthchecks hook (start, finish, fail,
// log) or the wrapper script (exit code + log).
func (m *Monitor) Ping(token string, kind PingKind, code *int, body, source string) error {
	ref, ok := m.Cfg.JobByToken(token)
	if !ok {
		return errors.New("unknown token")
	}
	now := m.Now()
	jobID := ref.Job.ID
	return m.Store.Update(func(s *store.State) {
		runs := s.Runs[jobID]
		var open *store.Run
		if n := len(runs); n > 0 && runs[n-1].Result == store.Running {
			open = runs[n-1]
		}
		switch kind {
		case PingStart:
			if open != nil { // the previous run never reported back
				t := now
				open.FinishedAt = nil
				open.Result = store.Failure
				open.Findings = append(open.Findings, store.Finding{Level: "error",
					Summary: "Lauf ohne Abschlussmeldung – abgebrochen oder Meldung verloren", Hint: "Logs auf dem Backup-Host prüfen."})
				open.ReceivedAt = t
			}
			r := &store.Run{ID: NewID(), JobID: jobID, StartedAt: &now, Result: store.Running, Source: source, ReceivedAt: now}
			r.AppendLog(body)
			s.Runs[jobID] = append(s.Runs[jobID], r)
		case PingLog:
			if open == nil && len(runs) > 0 {
				open = runs[len(runs)-1]
			}
			if open == nil {
				open = &store.Run{ID: NewID(), JobID: jobID, Result: store.Running, Source: source}
				s.Runs[jobID] = append(s.Runs[jobID], open)
			}
			open.AppendLog(body)
			open.ReceivedAt = now
		default:
			r := open
			if r == nil {
				r = &store.Run{ID: NewID(), JobID: jobID, Source: source}
				s.Runs[jobID] = append(s.Runs[jobID], r)
			}
			r.FinishedAt = &now
			r.ReceivedAt = now
			r.Source = source
			r.AppendLog(body)
			r.Findings, r.Result, r.ExitCode = nil, "", code
			findings, hasWarn, hasErr, stats := borg.AnalyzeLog(r.Log)
			r.Findings, r.Stats = findings, stats
			switch kind {
			case PingFail:
				r.Result = store.Failure
			case PingExit:
				res, txt := borg.ClassifyExit(*code)
				r.Result = res
				if res != store.Success { // the cause from the log first, the exit code after it
					r.Findings = append(r.Findings, store.Finding{Level: levelOf(res), Summary: txt})
				}
			default:
				r.Result = store.Success
			}
			if r.Result == store.Success && (hasWarn || hasErr) {
				r.Result = store.Warning
				if hasErr {
					r.Findings = append(r.Findings, store.Finding{Level: "warning",
						Summary: "Erfolg gemeldet, aber Fehlermeldungen im Log", Hint: "Log prüfen – evtl. betrifft der Fehler nur einen Teil (z. B. einen Hook)."})
				}
			}
			if r.Result == store.Failure && len(r.Findings) == 0 {
				r.Findings = []store.Finding{{Level: "error", Summary: "borgmatic hat einen Fehler gemeldet",
					Hint: "Im borgmatic-Hook „send_logs“ aktivieren, damit hier die Ursache steht."}}
			}
		}
	})
}

func levelOf(r store.Result) string {
	if r == store.Warning {
		return "warning"
	}
	return "error"
}

// --- views ------------------------------------------------------------------

type RunView struct {
	ID          string          `json:"id"`
	Result      store.Result    `json:"result"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	DurationSec *float64        `json:"duration_seconds,omitempty"`
	ExitCode    *int            `json:"exit_code,omitempty"`
	Source      string          `json:"source"`
	Findings    []store.Finding `json:"findings,omitempty"`
	Stats       *store.Stats    `json:"stats,omitempty"`
	HasLog      bool            `json:"has_log"`
	ReceivedAt  time.Time       `json:"received_at"`
}

func runView(r *store.Run) *RunView {
	if r == nil {
		return nil
	}
	v := &RunView{ID: r.ID, Result: r.Result, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, ExitCode: r.ExitCode,
		Source: r.Source, Findings: r.Findings, Stats: r.Stats, HasLog: r.Log != "", ReceivedAt: r.ReceivedAt}
	if d := r.Duration(); d != nil {
		s := d.Seconds()
		v.DurationSec = &s
	}
	return v
}

type ArchivesView struct {
	Checked    bool            `json:"checked"`
	CheckedAt  *time.Time      `json:"checked_at,omitempty"`
	LastGoodAt *time.Time      `json:"last_good_at,omitempty"`
	OK         bool            `json:"ok"`
	Error      *store.Finding  `json:"error,omitempty"`
	Count      int             `json:"count"`
	Latest     *store.Archive  `json:"latest,omitempty"`
	Encryption string          `json:"encryption,omitempty"`
	Sizes      *store.Sizes    `json:"sizes,omitempty"`
	SizesNote  string          `json:"sizes_note,omitempty"`
	Recent     []store.Archive `json:"recent,omitempty"`
}

type DayCell struct {
	Date   string `json:"date"`
	Result string `json:"result"` // success | warning | failure | running | archive | none
	Runs   int    `json:"runs"`
}

type Row struct {
	RepoID         string          `json:"repo_id"`
	JobID          string          `json:"job_id"`
	Host           string          `json:"host"`
	Job            string          `json:"job"`
	Description    string          `json:"description,omitempty"`
	Repo           string          `json:"repo"`
	Location       string          `json:"location,omitempty"`
	Queried        bool            `json:"queried"`
	PingConfigured bool            `json:"ping_configured"`
	Status         Status          `json:"status"`
	IntervalSec    float64         `json:"interval_seconds"`
	ToleranceSec   float64         `json:"tolerance_seconds"`
	LastAttempt    *RunView        `json:"last_attempt,omitempty"`
	LastFinished   *RunView        `json:"last_finished,omitempty"`
	LastOK         *RunView        `json:"last_ok,omitempty"`
	Running        *RunView        `json:"running,omitempty"`
	LastSuccessAt  *time.Time      `json:"last_success_at,omitempty"`
	SuccessSource  string          `json:"success_source,omitempty"`
	AgeSec         *float64        `json:"age_seconds,omitempty"`
	Archives       ArchivesView    `json:"archives"`
	Restore        RestoreStatus   `json:"restore"`
	RestoreEnabled bool            `json:"restore_enabled"`
	History        []DayCell       `json:"history"`
	Check          CheckStatus     `json:"check"`
	CheckPlan      string          `json:"check_plan,omitempty"`
	AutoRestore    string          `json:"auto_restore,omitempty"`
	Drift          *Drift          `json:"drift,omitempty"`
	ExpectedPaths  []string        `json:"expected_paths,omitempty"`
	Coverage       *store.Coverage `json:"coverage,omitempty"`
}

func checkPlan(c config.Check) string {
	if !c.Enabled() {
		return ""
	}
	s := "alle " + HumanInterval(c.Schedule.D()) + ", " + modeText(c.Mode)
	if c.Last > 0 && c.Mode != "repository" {
		s += fmt.Sprintf(" (neueste %d Archive)", c.Last)
	}
	if c.VerifyData {
		s += ", mit Datenprüfung"
	}
	if c.Window != "" {
		s += ", Zeitfenster " + c.Window
	}
	return s
}

func autoRestorePlan(rt config.RestoreTest, enabled bool) string {
	if !enabled || rt.Schedule <= 0 {
		return ""
	}
	s := fmt.Sprintf("alle %s je %d zufällige Dateien aus %s", HumanInterval(rt.Schedule.D()), rt.SampleFiles, strings.Join(rt.SamplePaths, ", "))
	if rt.Window != "" {
		s += ", Zeitfenster " + rt.Window
	}
	return s
}

type Summary struct {
	Total   int            `json:"total"`
	Levels  map[Level]int  `json:"levels"`
	Restore map[string]int `json:"restore"`
	Checks  map[string]int `json:"checks"`
	// Oldest information any status rests on – how current the dashboard is.
	OldestUpdate *time.Time `json:"oldest_update,omitempty"`
	NewestUpdate *time.Time `json:"newest_update,omitempty"`
}

type Overview struct {
	Demo        bool       `json:"demo"`
	GeneratedAt time.Time  `json:"generated_at"`
	Borg        *borg.Info `json:"borg"`
	Prlimit     bool       `json:"prlimit"`
	Summary     Summary    `json:"summary"`
	Rows        []Row      `json:"rows"`
}

const historyDays = 14

func (m *Monitor) rows(st *store.State, now time.Time) []Row {
	var rows []Row
	for _, j := range m.Cfg.Jobs() {
		runs := st.Runs[j.Job.ID]
		for i := range j.Job.Repositories {
			repo := &j.Job.Repositories[i]
			snap := st.Repos[repo.ID]
			var checks []*store.CheckResult
			for _, c := range st.Checks {
				if c.RepoID == repo.ID {
					checks = append(checks, c)
				}
			}
			cs := EvaluateCheck(now, checks, repo.Check)
			e := Evaluate(EvalInput{Now: now, Interval: j.Job.Interval.D(), Tolerance: j.Job.Tolerance.D(),
				RunningWarnAfter: m.Cfg.Defaults.RunningWarnAfter.D(), RefreshInterval: m.Cfg.Borg.RefreshInterval.D(),
				Queried: repo.Queried(), PingConfigured: j.Job.PingToken != "", Runs: runs, Snap: snap,
				ExpectedPaths: j.Job.ExpectedPaths, DriftThreshold: j.Job.DriftThreshold, Check: &cs})
			var tests []*store.RestoreTest
			for _, t := range st.RestoreTests {
				if t.RepoID == repo.ID {
					tests = append(tests, t)
				}
			}
			enabled := j.Job.RestoreEnabled(m.Cfg.Restore.Enabled)
			row := Row{RepoID: repo.ID, JobID: j.Job.ID, Host: j.Host, Job: j.Job.Name, Description: j.Job.Description,
				Repo: repo.Name, Location: repo.Location, Queried: repo.Queried(), PingConfigured: j.Job.PingToken != "",
				Status: e.Status, IntervalSec: j.Job.Interval.D().Seconds(), ToleranceSec: j.Job.Tolerance.D().Seconds(),
				LastAttempt: runView(e.LastAttempt), LastFinished: runView(e.LastFinished), LastOK: runView(e.LastOK),
				Running: runView(e.Running), LastSuccessAt: e.LastSuccessAt, SuccessSource: e.SuccessSource,
				Restore: EvaluateRestore(now, tests, j.Job.RestoreTest.MaxAge.D(), enabled), RestoreEnabled: enabled,
				Check: cs, Drift: computeDrift(runs, j.Job.DriftThreshold), ExpectedPaths: j.Job.ExpectedPaths,
				CheckPlan: checkPlan(repo.Check), AutoRestore: autoRestorePlan(j.Job.RestoreTest, enabled)}
			if snap != nil {
				row.Coverage = snap.Coverage
			}
			if e.LastSuccessAt != nil {
				a := now.Sub(*e.LastSuccessAt).Seconds()
				row.AgeSec = &a
			}
			row.Archives = archivesView(snap, e.LastOK, len(j.Job.Repositories))
			row.History = history(now, runs, snap)
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		ra, rb := rows[a].Status.Level.Rank(), rows[b].Status.Level.Rank()
		if ra != rb {
			return ra > rb
		}
		return strings.ToLower(rows[a].Host+"/"+rows[a].Job+"/"+rows[a].Repo) < strings.ToLower(rows[b].Host+"/"+rows[b].Job+"/"+rows[b].Repo)
	})
	return rows
}

func archivesView(snap *store.RepoSnapshot, lastOK *store.Run, reposInJob int) ArchivesView {
	v := ArchivesView{}
	if snap != nil {
		t := snap.CheckedAt
		v.Checked, v.CheckedAt, v.LastGoodAt, v.OK, v.Error = true, &t, snap.LastGoodAt, snap.OK, snap.Error
		v.Count, v.Latest, v.Encryption, v.Sizes, v.Recent = snap.ArchiveCount, snap.Latest, snap.Encryption, snap.Sizes, snap.Recent
		if snap.SizesError != "" {
			v.SizesNote = "borg info: " + snap.SizesError
		}
	}
	// sizes from the run log (--stats) when newer or nothing else is known
	if lastOK != nil && lastOK.Stats != nil && lastOK.Stats.Original > 0 && (v.Sizes == nil || (lastOK.FinishedAt != nil && lastOK.FinishedAt.After(v.Sizes.At))) {
		st := lastOK.Stats
		v.Sizes = &store.Sizes{At: *lastOK.FinishedAt, Source: "borgmatic --stats", Original: st.Original, Compressed: st.Compressed,
			Deduplicated: st.Deduplicated, AllOriginal: st.AllOriginal, AllDeduplicated: st.AllDeduplicated}
		if reposInJob > 1 {
			v.SizesNote = "aus dem Lauf-Log; bei mehreren Repositorys im Job gilt der zuletzt protokollierte Wert"
		}
	}
	if v.Sizes == nil && v.SizesNote == "" {
		v.SizesNote = "keine Größenangaben verfügbar – borgmatic mit --stats laufen lassen oder borg.sizes_interval setzen"
	}
	return v
}

func history(now time.Time, runs []*store.Run, snap *store.RepoSnapshot) []DayCell {
	rank := map[string]int{"none": 0, "archive": 1, "running": 2, "success": 3, "warning": 4, "failure": 5}
	cells := make([]DayCell, historyDays)
	idx := map[string]int{}
	day0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for i := 0; i < historyDays; i++ {
		d := day0.AddDate(0, 0, i-historyDays+1).Format("2006-01-02")
		cells[i] = DayCell{Date: d, Result: "none"}
		idx[d] = i
	}
	set := func(t time.Time, res string, run bool) {
		i, ok := idx[t.In(now.Location()).Format("2006-01-02")]
		if !ok {
			return
		}
		if run {
			cells[i].Runs++
		}
		if rank[res] > rank[cells[i].Result] {
			cells[i].Result = res
		}
	}
	for _, r := range runs {
		t := r.ReceivedAt
		if r.StartedAt != nil {
			t = *r.StartedAt
		}
		set(t, string(r.Result), true)
	}
	if len(runs) == 0 && snap != nil {
		for _, a := range snap.Recent {
			set(a.Start, "archive", false)
		}
	}
	return cells
}

func (m *Monitor) Overview(ctx context.Context) Overview {
	now := m.Now()
	ov := Overview{Demo: m.Demo, GeneratedAt: now, Borg: m.Backend.Detect(ctx), Prlimit: borg.HasPrlimit() || m.Demo,
		Summary: Summary{Levels: map[Level]int{}, Restore: map[string]int{}, Checks: map[string]int{}}}
	m.Store.Read(func(st *store.State) { ov.Rows = m.rows(st, now) })
	for _, r := range ov.Rows {
		ov.Summary.Total++
		ov.Summary.Levels[r.Status.Level]++
		ov.Summary.Restore[r.Restore.State]++
		ov.Summary.Checks[r.Check.State]++
		if u := r.Status.UpdatedAt; u != nil {
			if ov.Summary.OldestUpdate == nil || u.Before(*ov.Summary.OldestUpdate) {
				ov.Summary.OldestUpdate = u
			}
			if ov.Summary.NewestUpdate == nil || u.After(*ov.Summary.NewestUpdate) {
				ov.Summary.NewestUpdate = u
			}
		}
	}
	return ov
}

type Detail struct {
	Row          Row                  `json:"row"`
	Checks       []*store.CheckResult `json:"checks"`
	Runs         []*RunView           `json:"runs"`
	RestoreTests []*store.RestoreTest `json:"restore_tests"`
	PingURL      string               `json:"ping_url,omitempty"`
	Limits       map[string]any       `json:"restore_limits"`
}

func (m *Monitor) Detail(repoID string) (*Detail, bool) {
	ref, repo, ok := m.Cfg.RepoByID(repoID)
	if !ok {
		return nil, false
	}
	now := m.Now()
	d := &Detail{}
	m.Store.Read(func(st *store.State) {
		for _, r := range m.rows(st, now) {
			if r.RepoID == repoID {
				d.Row = r
			}
		}
		runs := st.Runs[ref.Job.ID]
		for i := len(runs) - 1; i >= 0 && len(d.Runs) < 60; i-- {
			d.Runs = append(d.Runs, runView(runs[i]))
		}
		for i := len(st.Checks) - 1; i >= 0 && len(d.Checks) < 20; i-- {
			if c := st.Checks[i]; c.RepoID == repo.ID {
				x := *c
				d.Checks = append(d.Checks, &x)
			}
		}
		for i := len(st.RestoreTests) - 1; i >= 0 && len(d.RestoreTests) < 30; i-- {
			if t := st.RestoreTests[i]; t.RepoID == repo.ID {
				c := *t
				c.Log = ""
				d.RestoreTests = append(d.RestoreTests, &c)
			}
		}
	})
	if ref.Job.PingToken != "" {
		base := strings.TrimRight(m.Cfg.PublicURL, "/")
		if base == "" {
			base = "http://<monitor-adresse>"
		}
		d.PingURL = base + "/ping/" + ref.Job.PingToken
	}
	d.Limits = map[string]any{"max_bytes": int64(m.Cfg.Restore.MaxBytes), "max_files": m.Cfg.Restore.MaxFiles,
		"timeout_seconds": m.Cfg.Restore.Timeout.D().Seconds(), "memory_max": int64(m.Cfg.Restore.MemoryMax),
		"reference": ref.Job.RestoreTest.ReferenceChecksums != "" || ref.Job.RestoreTest.CompareRoot != "" || m.Demo}
	return d, true
}

// RunLog returns the original log of one run.
func (m *Monitor) RunLog(runID string) (string, bool, bool) {
	var log string
	var cut, ok bool
	m.Store.Read(func(st *store.State) {
		for _, runs := range st.Runs {
			for _, r := range runs {
				if r.ID == runID {
					log, cut, ok = r.Log, r.LogCut, true
				}
			}
		}
	})
	return log, cut, ok
}

// JobRunning: a backup of this job is reported as running right now.
func (m *Monitor) JobRunning(jobID string) bool {
	running := false
	m.Store.Read(func(st *store.State) {
		runs := st.Runs[jobID]
		if n := len(runs); n > 0 && runs[n-1].Result == store.Running && runs[n-1].StartedAt != nil &&
			m.Now().Sub(*runs[n-1].StartedAt) < m.Cfg.Defaults.RunningWarnAfter.D() {
			running = true
		}
	})
	return running
}
