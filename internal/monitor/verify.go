package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/borg"
	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

// --- coverage: did every expected source make it into the newest archive? ---

// checkCoverage lists each expected path in the newest archive (a few
// entries only) – cheap, and it catches a volume that was not mounted.
func (m *Monitor) checkCoverage(ctx context.Context, job *config.Job, repo *config.Repository, snap *store.RepoSnapshot, old *store.RepoSnapshot) {
	if len(job.ExpectedPaths) == 0 || snap.Latest == nil {
		return
	}
	if old != nil && old.Coverage != nil && old.Coverage.Archive == snap.Latest.Name && old.Coverage.Error == "" {
		snap.Coverage = old.Coverage // already checked this archive
		return
	}
	cov := &store.Coverage{Archive: snap.Latest.Name, CheckedAt: m.Now()}
	for _, p := range job.ExpectedPaths {
		items, _, berr := m.Backend.Files(ctx, repo, snap.Latest.Name, p, 3)
		if berr != nil {
			cov.Error = berr.Finding.Summary
			break
		}
		cp := store.CoveragePath{Path: p, Entries: len(items)}
		for _, it := range items {
			if it.Path == p || strings.HasPrefix(it.Path, p+"/") {
				cp.Present = true
			}
		}
		// only the directory itself, nothing inside: empty mount point
		cp.Empty = cp.Present && len(items) == 1 && items[0].Type == "dir"
		cov.Paths = append(cov.Paths, cp)
	}
	snap.Coverage = cov
}

// coverageReasons turns the coverage of the newest archive into reasons.
func coverageReasons(job []string, snap *store.RepoSnapshot) []Reason {
	if len(job) == 0 || snap == nil || snap.Latest == nil {
		return nil
	}
	c := snap.Coverage
	if c == nil || c.Archive != snap.Latest.Name {
		return []Reason{{Level: Info, Text: "Abdeckung des neuesten Archivs wird noch geprüft."}}
	}
	if c.Error != "" {
		return []Reason{{Level: Unknown, Text: "Abdeckung nicht prüfbar: " + c.Error}}
	}
	var out []Reason
	for _, p := range c.Paths {
		switch {
		case !p.Present:
			out = append(out, Reason{Level: Err, Text: "Erwartete Quelle fehlt im neuesten Archiv: " + p.Path,
				Hint: "Volume beim Backup nicht eingebunden oder Pfad in borgmatic geändert? Die Sicherung ist unvollständig."})
		case p.Empty:
			out = append(out, Reason{Level: Warn, Text: "Erwartete Quelle ist im neuesten Archiv leer: " + p.Path,
				Hint: "Leerer Mount-Punkt – war das Laufwerk/Volume beim Backup eingehängt?"})
		}
	}
	return out
}

// --- drift: much less than usual backed up ------------------------------------

// Drift compares the newest successful run with the median of the runs
// before it (files counted by borg --stats).
type Drift struct {
	Files      int64   `json:"files"`
	Median     int64   `json:"median"`
	ChangePct  float64 `json:"change_percent"`
	Suspicious bool    `json:"suspicious"`
}

func computeDrift(runs []*store.Run, threshold float64) *Drift {
	var files []int64
	for i := len(runs) - 1; i >= 0 && len(files) < 8; i-- {
		r := runs[i]
		if (r.Result == store.Success || r.Result == store.Warning) && r.Stats != nil && r.Stats.Files > 0 {
			files = append(files, r.Stats.Files)
		}
	}
	if len(files) < 3 {
		return nil
	}
	prev := append([]int64(nil), files[1:]...)
	sort.Slice(prev, func(a, b int) bool { return prev[a] < prev[b] })
	med := prev[len(prev)/2]
	if med == 0 {
		return nil
	}
	d := &Drift{Files: files[0], Median: med, ChangePct: (float64(files[0]) - float64(med)) / float64(med) * 100}
	d.Suspicious = d.ChangePct <= -threshold*100
	return d
}

// --- integrity check (borg check) ------------------------------------------------

type CheckStatus struct {
	State string             `json:"state"` // passed | failed | stale | never | running | disabled
	Label string             `json:"label"`
	Text  string             `json:"text"`
	Last  *store.CheckResult `json:"last,omitempty"`
}

func EvaluateCheck(now time.Time, results []*store.CheckResult, c config.Check) CheckStatus {
	var last *store.CheckResult
	if n := len(results); n > 0 {
		last = results[n-1]
	}
	cs := CheckStatus{Last: last}
	switch {
	case last == nil && !c.Enabled():
		cs.State, cs.Label, cs.Text = "disabled", "nicht eingerichtet", "Keine Integritätsprüfung geplant (check.schedule)."
	case last == nil:
		cs.State, cs.Label, cs.Text = "never", "noch nicht geprüft", "Die erste Prüfung läuft im nächsten Zeitfenster."
	case last.State == "running":
		cs.State, cs.Label, cs.Text = "running", "läuft", "borg check läuft seit "+fmtTime(last.StartedAt)+"."
	case last.State == "failed":
		cs.State, cs.Label, cs.Text = "failed", "fehlgeschlagen", "borg check am "+fmtTime(last.StartedAt)+" fehlgeschlagen."
	case c.Enabled() && last.FinishedAt != nil && now.Sub(*last.FinishedAt) > 2*c.Schedule.D()+24*time.Hour:
		cs.State, cs.Label, cs.Text = "stale", "überfällig", "Letzte erfolgreiche Prüfung vor "+ago(now, *last.FinishedAt)+"."
	default:
		cs.State, cs.Label, cs.Text = "passed", "bestanden", "borg check ("+modeText(last.Mode)+") am "+fmtTime(last.StartedAt)+" ohne Befund."
	}
	return cs
}

func modeText(m string) string {
	switch m {
	case "archives":
		return "Archive"
	case "both":
		return "Repository und Archive"
	}
	return "Repository"
}

var ErrCheckBusy = errors.New("für dieses Repository läuft bereits eine Prüfung")

// RunCheck starts borg check in the background (trigger: zeitplan | manuell).
func (m *Monitor) RunCheck(repoID, trigger string) (*store.CheckResult, error) {
	ref, repo, ok := m.Cfg.RepoByID(repoID)
	if !ok {
		return nil, errors.New("unbekanntes Repository")
	}
	if !repo.Queried() {
		return nil, errors.New("Repository wird nicht abgefragt (query: false)")
	}
	if m.JobRunning(ref.Job.ID) {
		return nil, errors.New("für diesen Job läuft gerade ein Backup – Prüfung später")
	}
	key := "check:" + repoID
	m.mu.Lock()
	if m.inflight[key] {
		m.mu.Unlock()
		return nil, ErrCheckBusy
	}
	m.inflight[key] = true
	m.mu.Unlock()
	cr := &store.CheckResult{ID: NewID(), RepoID: repoID, StartedAt: m.Now(), State: "running", Mode: repo.Check.Mode, Trigger: trigger}
	_ = m.Store.Update(func(s *store.State) { s.Checks = append(s.Checks, cr) })
	snapshot := *cr
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.inflight, key)
			m.mu.Unlock()
		}()
		stderr, code, err := m.Backend.Check(context.Background(), repo)
		now := m.Now()
		_ = m.Store.Update(func(s *store.State) {
			for _, x := range s.Checks {
				if x.ID != cr.ID {
					continue
				}
				x.FinishedAt, x.ExitCode = &now, &code
				x.Log = tail(stderr, 64<<10)
				if err == nil && code == 0 {
					x.State = "passed"
					return
				}
				x.State = "failed"
				f := borg.Explain("", firstNonEmptyStr(errString(err), "")+"\n"+stderr)
				f.Level = "error"
				_, txt := borg.ClassifyExit(code)
				x.Findings = []store.Finding{f, {Level: "error", Summary: txt}}
			}
		})
		m.Log.Info("borg check finished", "repo", repoID, "code", code)
	}()
	return &snapshot, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstNonEmptyStr(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// checksDue starts scheduled checks: due, inside the window, no backup running.
func (m *Monitor) checksDue() {
	now := m.Now()
	for _, j := range m.Cfg.Jobs() {
		for i := range j.Job.Repositories {
			repo := &j.Job.Repositories[i]
			if !repo.Check.Enabled() || !repo.Queried() || !config.InWindow(repo.Check.Window, now, m.Cfg.Location) {
				continue
			}
			var last *store.CheckResult
			m.Store.Read(func(s *store.State) {
				for _, c := range s.Checks {
					if c.RepoID == repo.ID {
						last = c
					}
				}
			})
			if last != nil && (last.State == "running" || now.Sub(last.StartedAt) < repo.Check.Schedule.D()) {
				continue
			}
			if _, err := m.RunCheck(repo.ID, "zeitplan"); err != nil {
				m.Log.Info("scheduled check postponed", "repo", repo.ID, "reason", err.Error())
			}
		}
	}
}

// RecoverChecks marks checks interrupted by a restart as failed.
func (m *Monitor) RecoverChecks() {
	_ = m.Store.Update(func(s *store.State) {
		for _, c := range s.Checks {
			if c.State == "running" {
				now := m.Now()
				c.State, c.FinishedAt = "failed", &now
				c.Findings = append(c.Findings, store.Finding{Level: "error", Summary: "Prüfung durch Neustart des Monitors abgebrochen"})
			}
		}
	})
}

// --- notifications ------------------------------------------------------------------

type Notifier interface {
	Send(ctx context.Context, title, message string, level Level, click string) error
}

// HTTPNotifier sends to ntfy and/or a JSON webhook.
type HTTPNotifier struct {
	Cfg    config.Notify
	Client *http.Client
}

func (n *HTTPNotifier) Send(ctx context.Context, title, message string, level Level, click string) error {
	var errs []string
	if n.Cfg.NtfyURL != "" {
		req, _ := http.NewRequestWithContext(ctx, "POST", n.Cfg.NtfyURL, strings.NewReader(message))
		req.Header.Set("Title", title)
		req.Header.Set("Tags", map[Level]string{Err: "rotating_light", Overdue: "hourglass", Warn: "warning", Unknown: "grey_question", OK: "white_check_mark"}[level])
		req.Header.Set("Priority", map[Level]string{Err: "high", Overdue: "high", Warn: "default", Unknown: "default", OK: "low"}[level])
		if click != "" {
			req.Header.Set("Click", click)
		}
		if n.Cfg.NtfyTokenFile != "" {
			if b, err := os.ReadFile(n.Cfg.NtfyTokenFile); err == nil {
				req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
			}
		}
		if err := do(n.Client, req); err != nil {
			errs = append(errs, "ntfy: "+err.Error())
		}
	}
	if n.Cfg.WebhookURL != "" {
		b, _ := json.Marshal(map[string]any{"title": title, "message": message, "level": level, "url": click})
		req, _ := http.NewRequestWithContext(ctx, "POST", n.Cfg.WebhookURL, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if err := do(n.Client, req); err != nil {
			errs = append(errs, "webhook: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func do(c *http.Client, req *http.Request) error {
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}

// notifyChanges sends a message when a row changes its level (worse, or back
// to OK). The first evaluation after start only records the levels.
func (m *Monitor) notifyChanges(ctx context.Context) {
	if m.Notifier == nil {
		return
	}
	minRank := Warn.Rank()
	if m.Cfg.Notify.MinLevel == "error" {
		minRank = Overdue.Rank()
	}
	ov := m.Overview(ctx)
	type msg struct {
		repo  string
		level Level
		title string
		text  string
	}
	var out []msg
	_ = m.Store.Update(func(s *store.State) {
		for _, r := range ov.Rows {
			prev, seen := s.Notified[r.RepoID]
			cur := string(r.Status.Level)
			if prev == cur {
				continue
			}
			s.Notified[r.RepoID] = cur
			if !seen {
				continue // first sight: no message
			}
			pr, cr := Level(prev).Rank(), r.Status.Level.Rank()
			name := r.Host + " / " + r.Job + " → " + r.Repo
			switch {
			case cr >= minRank:
				out = append(out, msg{r.RepoID, r.Status.Level, r.Status.Label + ": " + name, firstText(r.Status.Reasons)})
			case r.Status.Level == OK && pr >= minRank:
				out = append(out, msg{r.RepoID, OK, "Wieder OK: " + name, firstText(r.Status.Reasons)})
			}
		}
	})
	base := strings.TrimRight(m.Cfg.PublicURL, "/")
	for _, x := range out {
		click := ""
		if base != "" {
			click = base + "/#/repo/" + x.repo
		}
		if err := m.Notifier.Send(ctx, x.title, x.text, x.level, click); err != nil {
			m.Log.Warn("notification failed", "err", err)
		}
	}
}

func firstText(rs []Reason) string {
	for _, r := range rs {
		if r.Level != Info {
			return r.Text
		}
	}
	if len(rs) > 0 {
		return rs[0].Text
	}
	return ""
}
