// Package store keeps what the monitor has observed – run reports, the last
// repository queries and restore tests – in one JSON file. Small, readable,
// no database server; writes are atomic (temp file + rename).
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	maxRunsPerJob     = 400
	maxRestoreTests   = 300
	maxLogBytes       = 256 << 10
	maxRecentArchives = 40
)

type Result string

const (
	Running Result = "running"
	Success Result = "success"
	Warning Result = "warning"
	Failure Result = "failure"
)

// Finding is one error or warning, summarised for people, with the original text.
type Finding struct {
	Level   string `json:"level"`   // error | warning
	Summary string `json:"summary"` // German, understandable
	Hint    string `json:"hint,omitempty"`
	Detail  string `json:"detail,omitempty"` // original line(s)
}

// Stats as borgmatic/borg print them with --stats (parsed from the run log).
type Stats struct {
	ArchiveName     string `json:"archive_name,omitempty"`
	Files           int64  `json:"files,omitempty"`
	Original        int64  `json:"original,omitempty"`
	Compressed      int64  `json:"compressed,omitempty"`
	Deduplicated    int64  `json:"deduplicated,omitempty"`
	AllOriginal     int64  `json:"all_original,omitempty"`
	AllCompressed   int64  `json:"all_compressed,omitempty"`
	AllDeduplicated int64  `json:"all_deduplicated,omitempty"`
}

// Run is one borgmatic run as reported to the ping endpoint.
type Run struct {
	ID         string     `json:"id"`
	JobID      string     `json:"job_id"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Result     Result     `json:"result"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Source     string     `json:"source"` // healthchecks-ping | wrapper
	Findings   []Finding  `json:"findings,omitempty"`
	Stats      *Stats     `json:"stats,omitempty"`
	Log        string     `json:"log,omitempty"`
	LogCut     bool       `json:"log_cut,omitempty"`
	ReceivedAt time.Time  `json:"received_at"`
}

func (r *Run) Duration() *time.Duration {
	if r.StartedAt == nil || r.FinishedAt == nil {
		return nil
	}
	d := r.FinishedAt.Sub(*r.StartedAt)
	return &d
}

// AppendLog keeps the tail of the log (the end holds the result).
func (r *Run) AppendLog(s string) {
	if s == "" {
		return
	}
	if r.Log != "" && r.Log[len(r.Log)-1] != '\n' {
		r.Log += "\n"
	}
	r.Log += s
	if len(r.Log) > maxLogBytes {
		r.Log = r.Log[len(r.Log)-maxLogBytes:]
		r.LogCut = true
	}
}

type Archive struct {
	Name     string     `json:"name"`
	Start    time.Time  `json:"start"`
	End      *time.Time `json:"end,omitempty"`
	Hostname string     `json:"hostname,omitempty"`
}

type Sizes struct {
	At              time.Time `json:"at"`
	Source          string    `json:"source"` // borg info | run log
	Original        int64     `json:"original,omitempty"`
	Compressed      int64     `json:"compressed,omitempty"`
	Deduplicated    int64     `json:"deduplicated,omitempty"`
	AllOriginal     int64     `json:"all_original,omitempty"`
	AllDeduplicated int64     `json:"all_deduplicated,omitempty"`
}

// RepoSnapshot is the last query of a repository (and the last good one).
type RepoSnapshot struct {
	RepoID        string     `json:"repo_id"`
	CheckedAt     time.Time  `json:"checked_at"`      // last attempt
	OK            bool       `json:"ok"`              // last attempt worked
	Error         *Finding   `json:"error,omitempty"` // of the last attempt
	LastGoodAt    *time.Time `json:"last_good_at,omitempty"`
	ArchiveCount  int        `json:"archive_count"`
	Latest        *Archive   `json:"latest,omitempty"`
	Recent        []Archive  `json:"recent,omitempty"` // newest first
	Encryption    string     `json:"encryption,omitempty"`
	LastModified  *time.Time `json:"last_modified,omitempty"`
	Sizes         *Sizes     `json:"sizes,omitempty"`
	SizesError    string     `json:"sizes_error,omitempty"`
	BorgVersion   string     `json:"borg_version,omitempty"`
	QueryDuration float64    `json:"query_seconds,omitempty"`
	Coverage      *Coverage  `json:"coverage,omitempty"`
}

// Coverage: were the expected sources in the newest archive?
type Coverage struct {
	Archive   string         `json:"archive"`
	CheckedAt time.Time      `json:"checked_at"`
	Paths     []CoveragePath `json:"paths"`
	Error     string         `json:"error,omitempty"`
}

type CoveragePath struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Entries int    `json:"entries"` // counted up to a small limit
	Empty   bool   `json:"empty"`
}

// CheckResult: one borg check run.
type CheckResult struct {
	ID         string     `json:"id"`
	RepoID     string     `json:"repo_id"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	State      string     `json:"state"` // running | passed | failed
	Mode       string     `json:"mode"`
	Trigger    string     `json:"trigger"` // zeitplan | manuell
	ExitCode   *int       `json:"exit_code,omitempty"`
	Findings   []Finding  `json:"findings,omitempty"`
	Log        string     `json:"log,omitempty"`
}

func (s *RepoSnapshot) SetArchives(list []Archive) {
	sort.Slice(list, func(i, j int) bool { return list[i].Start.After(list[j].Start) })
	s.ArchiveCount = len(list)
	s.Latest = nil
	if len(list) > 0 {
		a := list[0]
		s.Latest = &a
	}
	if len(list) > maxRecentArchives {
		list = list[:maxRecentArchives]
	}
	s.Recent = list
}

type RestoreFile struct {
	Path        string `json:"path"`
	Type        string `json:"type"` // file | dir | symlink | other
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
	Restored    bool   `json:"restored"`
	SizeOK      *bool  `json:"size_ok,omitempty"`
	Reference   string `json:"reference,omitempty"` // match | mismatch | missing | changed-since-backup | -
	ReferenceBy string `json:"reference_by,omitempty"`
	Note        string `json:"note,omitempty"`
}

// RestoreTest documents one controlled sample restore.
type RestoreTest struct {
	ID          string        `json:"id"`
	RepoID      string        `json:"repo_id"`
	JobID       string        `json:"job_id"`
	Archive     string        `json:"archive"`
	ArchiveTime *time.Time    `json:"archive_time,omitempty"`
	Paths       []string      `json:"paths"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	StartedBy   string        `json:"started_by"`
	State       string        `json:"state"`  // running | passed | passed-unverified | failed
	Target      string        `json:"target"` // temporary directory (removed afterwards unless keep_files)
	Removed     bool          `json:"removed"`
	Files       []RestoreFile `json:"files,omitempty"`
	Bytes       int64         `json:"bytes"`
	FileCount   int           `json:"file_count"`
	Verified    int           `json:"verified"` // files compared with an independent reference
	Findings    []Finding     `json:"findings,omitempty"`
	Log         string        `json:"log,omitempty"`
	Limits      string        `json:"limits"`
}

type State struct {
	Runs         map[string][]*Run        `json:"runs"`  // job id → newest last
	Repos        map[string]*RepoSnapshot `json:"repos"` // repo id
	RestoreTests []*RestoreTest           `json:"restore_tests"`
	Checks       []*CheckResult           `json:"checks,omitempty"`
	Notified     map[string]string        `json:"notified,omitempty"` // repo id → last notified level
}

type Store struct {
	mu    sync.RWMutex
	path  string
	state State
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, state: State{Runs: map[string][]*Run{}, Repos: map[string]*RepoSnapshot{}, Notified: map[string]string{}}}
	if path == "" {
		return s, nil // in memory (demo, tests)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.state); err != nil {
		return nil, err
	}
	if s.state.Runs == nil {
		s.state.Runs = map[string][]*Run{}
	}
	if s.state.Repos == nil {
		s.state.Repos = map[string]*RepoSnapshot{}
	}
	if s.state.Notified == nil {
		s.state.Notified = map[string]string{}
	}
	return s, nil
}

// Read gives fn a consistent view; fn must not keep references.
func (s *Store) Read(fn func(*State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.state)
}

// Update changes the state and writes it to disk.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	s.trim()
	return s.save()
}

func (s *Store) trim() {
	for id, runs := range s.state.Runs {
		if len(runs) > maxRunsPerJob {
			s.state.Runs[id] = append([]*Run(nil), runs[len(runs)-maxRunsPerJob:]...)
		}
	}
	if n := len(s.state.Checks); n > maxRestoreTests {
		s.state.Checks = append([]*CheckResult(nil), s.state.Checks[n-maxRestoreTests:]...)
	}
	if n := len(s.state.RestoreTests); n > maxRestoreTests {
		s.state.RestoreTests = append([]*RestoreTest(nil), s.state.RestoreTests[n-maxRestoreTests:]...)
	}
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(&s.state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), s.path)
}
