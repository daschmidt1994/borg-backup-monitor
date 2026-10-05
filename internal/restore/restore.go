// Package restore runs controlled sample restores: selected files of one
// archive into a freshly created, private temporary directory – never over
// original files, never into productive directories. Restored files are only
// read (hashed), never executed. A passed sample test does not prove that
// all data can be restored.
package restore

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/borg"
	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/monitor"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

const (
	maxPaths   = 50
	planTTL    = 15 * time.Minute
	Disclaimer = "Stichprobe: Ein bestandener Test zeigt, dass genau diese Dateien aus genau diesem Archiv wiederhergestellt werden konnten. Er garantiert nicht, dass alle Daten wiederherstellbar sind."
)

type Manager struct {
	Cfg     *config.Config
	Store   *store.Store
	Backend borg.Backend
	Mon     *monitor.Monitor

	mu      sync.Mutex
	running bool
	plans   map[string]*Plan
}

func New(cfg *config.Config, st *store.Store, b borg.Backend, mon *monitor.Monitor) *Manager {
	return &Manager{Cfg: cfg, Store: st, Backend: b, Mon: mon, plans: map[string]*Plan{}}
}

// Plan is shown before the start: what, where to, and within which limits.
type Plan struct {
	ID          string      `json:"id"`
	RepoID      string      `json:"repo_id"`
	JobID       string      `json:"job_id"`
	Archive     string      `json:"archive"`
	ArchiveTime *time.Time  `json:"archive_time,omitempty"`
	Paths       []string    `json:"paths"`
	Items       []borg.Item `json:"items"`
	Bytes       int64       `json:"bytes"`
	Files       int         `json:"files"`
	Target      string      `json:"target"`
	Limits      Limits      `json:"limits"`
	Reference   string      `json:"reference"`
	Problems    []string    `json:"problems,omitempty"`
	OK          bool        `json:"ok"`
	Disclaimer  string      `json:"disclaimer"`
	CreatedAt   time.Time   `json:"created_at"`
	ItemsCut    bool        `json:"items_cut"`
}

type Limits struct {
	MaxBytes       int64   `json:"max_bytes"`
	MaxFiles       int     `json:"max_files"`
	TimeoutSeconds float64 `json:"timeout_seconds"`
	MemoryMax      int64   `json:"memory_max"`
	MemoryEnforced bool    `json:"memory_enforced"`
	KeepFiles      bool    `json:"keep_files"`
}

func (m *Manager) limits() Limits {
	return Limits{MaxBytes: int64(m.Cfg.Restore.MaxBytes), MaxFiles: m.Cfg.Restore.MaxFiles,
		TimeoutSeconds: m.Cfg.Restore.Timeout.D().Seconds(), MemoryMax: int64(m.Cfg.Restore.MemoryMax),
		MemoryEnforced: borg.HasPrlimit(), KeepFiles: m.Cfg.Restore.KeepFiles}
}

func newID() string { return monitor.NewID() }

func validArchiveName(s string) bool {
	return s != "" && len(s) <= 255 && !strings.HasPrefix(s, "-") && !strings.ContainsAny(s, "/\x00\n\r")
}

func describeReference(rt config.RestoreTest, demo bool) string {
	var parts []string
	if rt.ReferenceChecksums != "" {
		parts = append(parts, "Prüfsummen-Datei "+filepath.Base(rt.ReferenceChecksums))
	}
	if rt.CompareRoot != "" {
		parts = append(parts, "Vergleich mit den Originaldateien unter "+rt.CompareRoot+" (nur lesend)")
	}
	if len(parts) == 0 {
		if demo {
			return "Demo-Referenz"
		}
		return "keine – geprüft werden nur Vollständigkeit, Größe und borgs eigene Integritätsprüfung beim Entpacken"
	}
	return strings.Join(parts, "; ")
}

// PlanTest checks a selection and describes the test without running it.
func (m *Manager) PlanTest(ctx context.Context, repoID, archive string, paths []string) (*Plan, error) {
	ref, repo, ok := m.Cfg.RepoByID(repoID)
	if !ok {
		return nil, errors.New("unbekanntes Repository")
	}
	if !ref.Job.RestoreEnabled(m.Cfg.Restore.Enabled) {
		return nil, errors.New("Restore-Tests sind für diesen Job nicht freigegeben (restore.enabled / restore_test.enabled)")
	}
	if !repo.Queried() {
		return nil, errors.New("Repository wird nicht abgefragt (query: false)")
	}
	if !validArchiveName(archive) {
		return nil, errors.New("ungültiger Archivname")
	}
	if len(paths) == 0 || len(paths) > maxPaths {
		return nil, fmt.Errorf("1 bis %d Pfade auswählen", maxPaths)
	}
	p := &Plan{ID: monitor.NewID(), RepoID: repoID, JobID: ref.Job.ID, Archive: archive, Limits: m.limits(),
		Reference: describeReference(ref.Job.RestoreTest, m.Mon.Demo), Disclaimer: Disclaimer, CreatedAt: time.Now(),
		Target: filepath.Join(m.Cfg.Restore.BaseDir, "test-<neu, zufällig>") + " (wird neu angelegt, nur für diesen Test, danach " + map[bool]string{true: "aufbewahrt", false: "gelöscht"}[m.Cfg.Restore.KeepFiles] + ")"}
	m.Store.Read(func(st *store.State) {
		if s := st.Repos[repoID]; s != nil {
			for _, a := range s.Recent {
				if a.Name == archive {
					t := a.Start
					p.ArchiveTime = &t
				}
			}
		}
	})
	seen := map[string]bool{}
	for _, raw := range paths {
		path := strings.TrimPrefix(strings.TrimSpace(raw), "/")
		if !borg.ValidArchivePath(path) {
			return nil, fmt.Errorf("ungültiger Pfad %q", raw)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		p.Paths = append(p.Paths, path)
		items, cut, berr := m.Backend.Files(ctx, repo, archive, path, p.Limits.MaxFiles+1)
		if berr != nil {
			return nil, fmt.Errorf("%s: %s", path, berr.Finding.Summary)
		}
		found := false
		for _, it := range items {
			if it.Path != path && !strings.HasPrefix(it.Path, path+"/") {
				continue
			}
			found = true
			p.Items = append(p.Items, it)
			if it.Type == "file" {
				p.Bytes += it.Size
				p.Files++
			}
		}
		if !found {
			p.Problems = append(p.Problems, "„"+path+"“ ist im Archiv nicht vorhanden")
		}
		if cut {
			p.ItemsCut = true
		}
	}
	if p.ItemsCut || len(p.Items) > p.Limits.MaxFiles {
		p.Problems = append(p.Problems, fmt.Sprintf("Auswahl umfasst mehr als %d Einträge – kleinere Auswahl treffen", p.Limits.MaxFiles))
	}
	if p.Bytes > p.Limits.MaxBytes {
		p.Problems = append(p.Problems, fmt.Sprintf("Auswahl ist %s groß – Grenze %s", humanBytes(p.Bytes), humanBytes(p.Limits.MaxBytes)))
	}
	if m.Mon.JobRunning(ref.Job.ID) {
		p.Problems = append(p.Problems, "Für diesen Job läuft gerade ein Backup – Test danach starten")
	}
	p.OK = len(p.Problems) == 0
	m.mu.Lock()
	for id, old := range m.plans {
		if time.Since(old.CreatedAt) > planTTL {
			delete(m.plans, id)
		}
	}
	m.plans[p.ID] = p
	m.mu.Unlock()
	return p, nil
}

// Start runs a planned test in the background. Only one test at a time.
func (m *Manager) Start(planID, user string) (*store.RestoreTest, error) {
	m.mu.Lock()
	p, ok := m.plans[planID]
	if !ok || time.Since(p.CreatedAt) > planTTL {
		m.mu.Unlock()
		return nil, errors.New("Plan abgelaufen – bitte neu prüfen")
	}
	if !p.OK {
		m.mu.Unlock()
		return nil, errors.New("Plan hat Probleme: " + strings.Join(p.Problems, "; "))
	}
	if m.running {
		m.mu.Unlock()
		return nil, errors.New("es läuft bereits ein Restore-Test")
	}
	m.running = true
	delete(m.plans, planID)
	m.mu.Unlock()

	ref, repo, _ := m.Cfg.RepoByID(p.RepoID)
	if m.Mon.JobRunning(ref.Job.ID) {
		m.done()
		return nil, errors.New("für diesen Job läuft gerade ein Backup")
	}
	if err := os.MkdirAll(m.Cfg.Restore.BaseDir, 0o700); err != nil {
		m.done()
		return nil, err
	}
	dir, err := os.MkdirTemp(m.Cfg.Restore.BaseDir, "test-")
	if err != nil {
		m.done()
		return nil, err
	}
	_ = os.Chmod(dir, 0o700)
	l := p.Limits
	t := &store.RestoreTest{ID: monitor.NewID(), RepoID: p.RepoID, JobID: p.JobID, Archive: p.Archive, ArchiveTime: p.ArchiveTime,
		Paths: p.Paths, StartedAt: time.Now(), StartedBy: user, State: "running", Target: dir,
		Limits: fmt.Sprintf("max. %s, %d Einträge, %s Laufzeit, Speicher %s%s", humanBytes(l.MaxBytes), l.MaxFiles,
			monitor.HumanDuration(time.Duration(l.TimeoutSeconds)*time.Second), humanBytes(l.MemoryMax),
			map[bool]string{true: "", false: " (nicht erzwungen – prlimit fehlt)"}[l.MemoryEnforced])}
	if err := m.Store.Update(func(s *store.State) { s.RestoreTests = append(s.RestoreTests, t) }); err != nil {
		m.done()
		os.RemoveAll(dir)
		return nil, err
	}
	snapshot := *t
	go func() {
		defer m.done()
		res := m.execute(ref, repo, p, dir, t.ID)
		_ = m.Store.Update(func(s *store.State) {
			for i, x := range s.RestoreTests {
				if x.ID == res.ID {
					s.RestoreTests[i] = res
				}
			}
		})
	}()
	return &snapshot, nil
}

func (m *Manager) done() {
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

// Recover marks tests interrupted by a restart as failed and removes their files.
func (m *Manager) Recover() {
	_ = m.Store.Update(func(s *store.State) {
		for _, t := range s.RestoreTests {
			if t.State == "running" {
				now := time.Now()
				t.State, t.FinishedAt = "failed", &now
				t.Findings = append(t.Findings, store.Finding{Level: "error", Summary: "Test durch Neustart des Monitors abgebrochen"})
				if m.inBase(t.Target) {
					t.Removed = os.RemoveAll(t.Target) == nil
				}
			}
		}
	})
}

func (m *Manager) inBase(dir string) bool {
	base, err1 := filepath.Abs(m.Cfg.Restore.BaseDir)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(base, d)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && strings.HasPrefix(filepath.Base(d), "test-")
}

func (m *Manager) execute(ref config.JobRef, repo *config.Repository, p *Plan, dir, id string) *store.RestoreTest {
	t := &store.RestoreTest{ID: id, RepoID: p.RepoID, JobID: p.JobID, Archive: p.Archive, ArchiveTime: p.ArchiveTime, Paths: p.Paths, Target: dir}
	m.Store.Read(func(s *store.State) {
		for _, x := range s.RestoreTests {
			if x.ID == id {
				c := *x
				t = &c
			}
		}
	})
	timeout := m.Cfg.Restore.Timeout.D()
	ctx, cancel := context.WithTimeout(context.Background(), timeout+10*time.Second)
	defer cancel()

	// watchdog: stop when the directory grows beyond the limit
	var overSize atomic.Bool
	stop := make(chan struct{})
	go func() {
		limit := p.Limits.MaxBytes + p.Limits.MaxBytes/20 + 1<<20
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if dirSize(dir) > limit {
					overSize.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	stderr, code, err := m.Backend.Extract(ctx, repo, borg.ExtractSpec{Archive: p.Archive, Paths: p.Paths, Dir: dir, Timeout: timeout, MemoryMax: p.Limits.MemoryMax})
	close(stop)
	t.Log = clip(stderr, 64<<10)
	var findings []store.Finding
	failed := false
	switch {
	case overSize.Load():
		failed = true
		findings = append(findings, store.Finding{Level: "error", Summary: "Größenlimit überschritten – Test abgebrochen"})
	case err != nil:
		failed = true
		f := borg.Explain("", err.Error()+"\n"+stderr)
		f.Level = "error"
		findings = append(findings, f)
	case code != 0:
		failed = true
		res, txt := borg.ClassifyExit(code)
		f := borg.Explain("", stderr)
		f.Level = "error"
		if res == store.Warning {
			f.Summary = "borg extract mit Warnung beendet: " + f.Summary
		}
		f.Detail = strings.TrimSpace(txt + "\n" + f.Detail)
		findings = append(findings, f)
	}

	files, restoredBytes, walkErr := inspect(dir)
	if walkErr != nil {
		failed = true
		findings = append(findings, store.Finding{Level: "error", Summary: "Wiederhergestellte Dateien nicht lesbar", Detail: walkErr.Error()})
	}
	refSums, refErr := loadChecksums(ref.Job.RestoreTest.ReferenceChecksums)
	if refErr != nil {
		findings = append(findings, store.Finding{Level: "warning", Summary: "Referenz-Prüfsummen nicht lesbar", Detail: refErr.Error()})
	}
	if m.Mon.Demo {
		refSums = demoReference(dir, files)
	}
	var out []store.RestoreFile
	verified, mismatches, missing := 0, 0, 0
	for _, it := range p.Items {
		rf := store.RestoreFile{Path: it.Path, Type: it.Type, Size: it.Size, Reference: "-"}
		got, ok := files[it.Path]
		if !ok {
			if it.Type != "other" {
				missing++
				rf.Note = "nicht wiederhergestellt"
			}
			out = append(out, rf)
			continue
		}
		rf.Restored = true
		if it.Type == "file" {
			sizeOK := got.size == it.Size
			rf.SizeOK, rf.SHA256 = &sizeOK, got.sha
			if !sizeOK {
				mismatches++
				rf.Note = fmt.Sprintf("Größe %d statt %d", got.size, it.Size)
			}
			if want, ok := refSums[it.Path]; ok {
				rf.ReferenceBy = "Prüfsummen-Datei"
				if strings.EqualFold(want, got.sha) {
					rf.Reference = "match"
					verified++
				} else {
					rf.Reference = "mismatch"
					mismatches++
				}
			} else if root := ref.Job.RestoreTest.CompareRoot; root != "" {
				rf.ReferenceBy = "Originaldatei"
				res, note := compareOriginal(root, it.Path, got.sha, p.ArchiveTime)
				rf.Reference, rf.Note = res, strings.TrimSpace(rf.Note+" "+note)
				switch res {
				case "match":
					verified++
				case "mismatch":
					mismatches++
				}
			}
		}
		if it.Type == "symlink" && got.typ != "symlink" {
			mismatches++
			rf.Note = "kein Symlink"
		}
		out = append(out, rf)
	}
	t.Files, t.Bytes, t.FileCount, t.Verified = out, restoredBytes, len(files), verified
	if missing > 0 {
		failed = true
		findings = append(findings, store.Finding{Level: "error", Summary: fmt.Sprintf("%d ausgewählte Einträge wurden nicht wiederhergestellt", missing)})
	}
	if mismatches > 0 {
		failed = true
		findings = append(findings, store.Finding{Level: "error", Summary: fmt.Sprintf("%d Abweichungen bei Größe oder Prüfsumme", mismatches)})
	}
	now := time.Now()
	t.FinishedAt = &now
	switch {
	case failed:
		t.State = "failed"
	case verified > 0:
		t.State = "passed"
	default:
		t.State = "passed-unverified"
		findings = append(findings, store.Finding{Level: "warning", Summary: "Inhalt nicht gegen eine unabhängige Referenz geprüft",
			Hint: "reference_checksums (sha256sum-Datei) oder compare_root für den Job eintragen."})
	}
	findings = append(findings, store.Finding{Level: "info", Summary: Disclaimer})
	t.Findings = findings
	if !m.Cfg.Restore.KeepFiles && m.inBase(dir) {
		t.Removed = os.RemoveAll(dir) == nil
	}
	return t
}

type restored struct {
	typ  string
	size int64
	sha  string
}

// inspect walks the restored tree without following symlinks, hashes regular
// files (opened with O_NOFOLLOW) and removes set-id and execute bits.
func inspect(root string) (map[string]restored, int64, error) {
	files := map[string]restored{}
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("Pfad außerhalb des Testverzeichnisses: %s", path)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			files[rel] = restored{typ: "symlink"}
		case info.IsDir():
			_ = os.Chmod(path, 0o700)
			files[rel] = restored{typ: "dir"}
		case info.Mode().IsRegular():
			_ = os.Chmod(path, info.Mode().Perm()&^0o111|0o600)
			sum, size, err := hashNoFollow(path)
			if err != nil {
				return err
			}
			total += size
			files[rel] = restored{typ: "file", size: size, sha: sum}
		default:
			files[rel] = restored{typ: "other"} // devices, fifos: never opened
		}
		return nil
	})
	return files, total, err
}

func hashNoFollow(path string) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if i, e := d.Info(); e == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}

// loadChecksums reads a sha256sum-style file ("<hash>  <path>" or "<hash> *<path>").
func loadChecksums(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if len(line) < 66 || strings.HasPrefix(line, "#") {
			continue
		}
		sum, rest := line[:64], strings.TrimLeft(line[64:], " ")
		rest = strings.TrimPrefix(rest, "*")
		rest = strings.TrimPrefix(strings.TrimPrefix(rest, "./"), "/")
		if _, err := hex.DecodeString(sum); err == nil && rest != "" {
			out[rest] = strings.ToLower(sum)
		}
	}
	return out, sc.Err()
}

// compareOriginal reads the live file below root (read-only) and compares.
func compareOriginal(root, rel, sha string, archiveTime *time.Time) (string, string) {
	base, err := filepath.Abs(root)
	if err != nil {
		return "-", "Vergleichsordner ungültig"
	}
	p := filepath.Join(base, filepath.FromSlash(rel))
	if r, err := filepath.Rel(base, p); err != nil || strings.HasPrefix(r, "..") {
		return "-", "Pfad außerhalb des Vergleichsordners"
	}
	info, err := os.Lstat(p)
	if err != nil {
		return "missing", "Original nicht vorhanden"
	}
	if !info.Mode().IsRegular() {
		return "-", "Original ist keine normale Datei"
	}
	sum, _, err := hashNoFollow(p)
	if err != nil {
		return "-", "Original nicht lesbar"
	}
	if sum == sha {
		return "match", ""
	}
	if archiveTime != nil && info.ModTime().After(*archiveTime) {
		return "changed-since-backup", "Original seit dem Backup geändert – Vergleich nicht aussagekräftig"
	}
	return "mismatch", "Inhalt weicht vom Original ab"
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return strings.Replace(fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp]), ".", ",", 1)
}

// demoReference: in demo mode the "independent" reference is generated.
func demoReference(dir string, files map[string]restored) map[string]string {
	out := map[string]string{}
	for p, f := range files {
		if f.typ == "file" {
			out[p] = f.sha
		}
	}
	return out
}
