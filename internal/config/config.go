// Package config reads the monitor's YAML configuration.
//
// The configuration describes what is monitored and how it is judged –
// never how backups are made: schedules and borgmatic configurations stay
// outside this application. Secrets (passphrases, SSH keys) are referenced
// by file path and only ever handed to the borg process.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration accepts Go durations plus days and weeks: "26h", "1d", "2w", "90m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

var dayWeek = regexp.MustCompile(`^(\d+(?:\.\d+)?)([dw])$`)

func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if m := dayWeek.FindStringSubmatch(s); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		unit := 24 * time.Hour
		if m[2] == "w" {
			unit *= 7
		}
		return time.Duration(f * float64(unit)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (examples: 90m, 26h, 1d, 2w)", s)
	}
	return d, nil
}

// Size accepts bytes with an optional unit: "500MB", "2GiB", "1048576".
type Size int64

func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*s = Size(v)
	return nil
}

var sizeRe = regexp.MustCompile(`(?i)^\s*(\d+(?:\.\d+)?)\s*([kmgt]?i?b?)?\s*$`)

func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (examples: 500MB, 2GiB)", s)
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	unit := strings.ToLower(m[2])
	mult := 1.0
	base := 1000.0
	if strings.Contains(unit, "i") {
		base = 1024
	}
	switch {
	case strings.HasPrefix(unit, "k"):
		mult = base
	case strings.HasPrefix(unit, "m"):
		mult = base * base
	case strings.HasPrefix(unit, "g"):
		mult = base * base * base
	case strings.HasPrefix(unit, "t"):
		mult = base * base * base * base
	}
	return int64(f * mult), nil
}

type Config struct {
	Listen    string `yaml:"listen"`
	DataDir   string `yaml:"data_dir"`
	PublicURL string `yaml:"public_url"` // shown in the ping URLs for borgmatic
	TimeZone  string `yaml:"time_zone"`  // for borg 1.x timestamps without offset

	Auth     Auth     `yaml:"auth"`
	Borg     Borg     `yaml:"borg"`
	Defaults Defaults `yaml:"defaults"`
	Restore  Restore  `yaml:"restore"`
	Notify   Notify   `yaml:"notify"`
	Hosts    []Host   `yaml:"hosts"`

	Location *time.Location `yaml:"-"`
}

type Auth struct {
	Users        []User `yaml:"users"`
	SessionHours int    `yaml:"session_hours"`
	SecureCookie *bool  `yaml:"secure_cookie"` // default: true when public_url is https
}

type User struct {
	Name         string `yaml:"name"`
	PasswordHash string `yaml:"password_hash"` // bcrypt, see: borg-monitor -hash-password
}

type Borg struct {
	Binary          string   `yaml:"binary"`
	Timeout         Duration `yaml:"timeout"`          // per read command
	RefreshInterval Duration `yaml:"refresh_interval"` // how often repositories are listed
	SizesInterval   Duration `yaml:"sizes_interval"`   // borg info (needs the cache) – 0 = never
	MaxParallel     int      `yaml:"max_parallel"`
	BypassLock      string   `yaml:"bypass_lock"` // auto | always | never
	MaxOutput       Size     `yaml:"max_output"`  // cap for JSON read from borg
	ManualCooldown  Duration `yaml:"manual_cooldown"`
}

type Defaults struct {
	Interval          Duration `yaml:"interval"`
	Tolerance         Duration `yaml:"tolerance"`
	RunningWarnAfter  Duration `yaml:"running_warn_after"`
	RestoreTestMaxAge Duration `yaml:"restore_test_max_age"`
}

type Restore struct {
	Enabled   bool     `yaml:"enabled"`
	BaseDir   string   `yaml:"base_dir"`
	MaxBytes  Size     `yaml:"max_bytes"`
	MaxFiles  int      `yaml:"max_files"`
	Timeout   Duration `yaml:"timeout"`
	MemoryMax Size     `yaml:"memory_max"` // address-space limit for borg (via prlimit, if installed)
	KeepFiles bool     `yaml:"keep_files"`
}

// Notify: messages when a status changes (ntfy and/or a JSON webhook).
type Notify struct {
	NtfyURL       string `yaml:"ntfy_url"`        // https://ntfy.example.com/backups
	NtfyTokenFile string `yaml:"ntfy_token_file"` // access token, read from a file
	WebhookURL    string `yaml:"webhook_url"`
	MinLevel      string `yaml:"min_level"` // warning (default) | error
}

func (n Notify) Enabled() bool { return n.NtfyURL != "" || n.WebhookURL != "" }

type Host struct {
	Name string `yaml:"name"`
	Jobs []Job  `yaml:"jobs"`
}

type Job struct {
	Name         string       `yaml:"name"`
	Description  string       `yaml:"description"`
	PingToken    string       `yaml:"ping_token"`
	Interval     Duration     `yaml:"interval"`
	Tolerance    Duration     `yaml:"tolerance"`
	Repositories []Repository `yaml:"repositories"`
	RestoreTest  RestoreTest  `yaml:"restore_test"`
	// Paths (as stored in the archive, e.g. "source/immich") that must be in
	// every new archive and not be empty – catches a volume missing at backup time.
	ExpectedPaths []string `yaml:"expected_paths"`
	// Warn when a run backs up this much less than usual (0.3 = 30 % fewer files).
	DriftThreshold float64 `yaml:"drift_threshold"`

	ID string `yaml:"-"`
}

type Repository struct {
	Name           string `yaml:"name"`
	Location       string `yaml:"location"`
	PassphraseFile string `yaml:"passphrase_file"`
	PassCommand    string `yaml:"passcommand"` // handed to borg as BORG_PASSCOMMAND
	SSHKey         string `yaml:"ssh_key"`
	KnownHosts     string `yaml:"known_hosts"`
	RemotePath     string `yaml:"remote_path"`
	Query          *bool  `yaml:"query"` // false: no borg access, run reports only
	Check          Check  `yaml:"check"`

	ID string `yaml:"-"`
}

// Check: scheduled integrity check (borg check). Off unless a schedule is
// set. borg check takes the repository lock – hence the time window, and it
// never starts while a backup of the job is reported as running.
type Check struct {
	Schedule   Duration `yaml:"schedule"`    // e.g. 7d; 0 = no automatic check
	Mode       string   `yaml:"mode"`        // repository | archives | both
	Last       int      `yaml:"last"`        // archives part: only the newest N archives (0 = all)
	VerifyData bool     `yaml:"verify_data"` // read all data (very slow)
	Window     string   `yaml:"window"`      // "01:00-05:00" (local time); empty = any time
	Timeout    Duration `yaml:"timeout"`
}

func (c Check) Enabled() bool { return c.Schedule > 0 }

func (r Repository) Queried() bool { return r.Query == nil || *r.Query }

type RestoreTest struct {
	Enabled            *bool    `yaml:"enabled"`
	ReferenceChecksums string   `yaml:"reference_checksums"` // sha256sum format
	CompareRoot        string   `yaml:"compare_root"`        // read-only originals on this host
	MaxAge             Duration `yaml:"max_age"`
	// Automatic sample restores: every Schedule, SampleFiles random files below
	// SamplePaths (archive paths) of the newest archive, inside Window.
	Schedule    Duration `yaml:"schedule"`
	SamplePaths []string `yaml:"sample_paths"`
	SampleFiles int      `yaml:"sample_files"`
	Window      string   `yaml:"window"`
}

// IDFor gives a stable, URL-safe id.
func IDFor(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6])
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	return &c, c.validate()
}

func (c *Config) applyDefaults() {
	def := func(d *Duration, v time.Duration) {
		if *d == 0 {
			*d = Duration(v)
		}
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.Auth.SessionHours <= 0 {
		c.Auth.SessionHours = 12
	}
	if c.Borg.Binary == "" {
		c.Borg.Binary = "borg"
	}
	def(&c.Borg.Timeout, 60*time.Second)
	def(&c.Borg.RefreshInterval, 15*time.Minute)
	def(&c.Borg.ManualCooldown, time.Minute)
	if c.Borg.MaxParallel <= 0 {
		c.Borg.MaxParallel = 2
	}
	if c.Borg.BypassLock == "" {
		c.Borg.BypassLock = "auto"
	}
	if c.Borg.MaxOutput <= 0 {
		c.Borg.MaxOutput = 32 << 20
	}
	def(&c.Defaults.Interval, 24*time.Hour)
	def(&c.Defaults.Tolerance, 6*time.Hour)
	def(&c.Defaults.RunningWarnAfter, 8*time.Hour)
	def(&c.Defaults.RestoreTestMaxAge, 30*24*time.Hour)
	if c.Restore.BaseDir == "" {
		c.Restore.BaseDir = filepath.Join(c.DataDir, "restore-tests")
	}
	if c.Restore.MaxBytes <= 0 {
		c.Restore.MaxBytes = 500 << 20
	}
	if c.Restore.MaxFiles <= 0 {
		c.Restore.MaxFiles = 1000
	}
	def(&c.Restore.Timeout, 10*time.Minute)
	if c.Restore.MemoryMax <= 0 {
		c.Restore.MemoryMax = 2 << 30
	}
	if c.Notify.MinLevel == "" {
		c.Notify.MinLevel = "warning"
	}
	if c.Auth.SecureCookie == nil {
		v := strings.HasPrefix(c.PublicURL, "https://")
		c.Auth.SecureCookie = &v
	}
	for h := range c.Hosts {
		for j := range c.Hosts[h].Jobs {
			job := &c.Hosts[h].Jobs[j]
			job.ID = IDFor(c.Hosts[h].Name, job.Name)
			def(&job.Interval, c.Defaults.Interval.D())
			def(&job.Tolerance, c.Defaults.Tolerance.D())
			def(&job.RestoreTest.MaxAge, c.Defaults.RestoreTestMaxAge.D())
			if job.DriftThreshold <= 0 {
				job.DriftThreshold = 0.3
			}
			if job.RestoreTest.SampleFiles <= 0 {
				job.RestoreTest.SampleFiles = 5
			}
			for i, p := range job.ExpectedPaths {
				job.ExpectedPaths[i] = strings.Trim(p, "/")
			}
			for r := range job.Repositories {
				job.Repositories[r].ID = IDFor(c.Hosts[h].Name, job.Name, job.Repositories[r].Name)
				ck := &job.Repositories[r].Check
				if ck.Mode == "" {
					ck.Mode = "repository"
				}
				def(&ck.Timeout, 6*time.Hour)
			}
		}
	}
}

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func (c *Config) validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	loc := time.Local
	if c.TimeZone != "" {
		l, err := time.LoadLocation(c.TimeZone)
		if err != nil {
			add("time_zone: %v", err)
		} else {
			loc = l
		}
	}
	c.Location = loc
	switch c.Notify.MinLevel {
	case "warning", "error":
	default:
		add("notify.min_level must be warning or error")
	}
	switch c.Borg.BypassLock {
	case "auto", "always", "never":
	default:
		add("borg.bypass_lock must be auto, always or never")
	}
	for _, u := range c.Auth.Users {
		if u.Name == "" || !strings.HasPrefix(u.PasswordHash, "$2") {
			add("auth.users: every user needs a name and a bcrypt password_hash (borg-monitor -hash-password)")
		}
	}
	seenHost := map[string]bool{}
	seenToken := map[string]bool{}
	for _, h := range c.Hosts {
		if h.Name == "" {
			add("hosts: name missing")
		}
		if seenHost[h.Name] {
			add("hosts: %q twice", h.Name)
		}
		seenHost[h.Name] = true
		seenJob := map[string]bool{}
		for _, j := range h.Jobs {
			where := h.Name + "/" + j.Name
			if j.Name == "" {
				add("%s: job name missing", h.Name)
			}
			if seenJob[j.Name] {
				add("%s: job twice", where)
			}
			seenJob[j.Name] = true
			if j.PingToken != "" {
				if !tokenRe.MatchString(j.PingToken) {
					add("%s: ping_token must be 16–128 characters A–Z, a–z, 0–9, _ or - (borg-monitor -new-token)", where)
				}
				if seenToken[j.PingToken] {
					add("%s: ping_token used twice", where)
				}
				seenToken[j.PingToken] = true
			}
			if _, _, err := ParseWindow(j.RestoreTest.Window); err != nil {
				add("%s: restore_test.window: %v", where, err)
			}
			if j.RestoreTest.Schedule > 0 && len(j.RestoreTest.SamplePaths) == 0 {
				add("%s: restore_test.schedule needs sample_paths", where)
			}
			if len(j.Repositories) == 0 {
				add("%s: at least one repository", where)
			}
			seenRepo := map[string]bool{}
			for _, r := range j.Repositories {
				if r.Name == "" {
					add("%s: repository name missing", where)
				}
				if seenRepo[r.Name] {
					add("%s: repository %q twice", where, r.Name)
				}
				seenRepo[r.Name] = true
				if r.Queried() && r.Location == "" {
					add("%s/%s: location missing (or set query: false)", where, r.Name)
				}
				if r.PassphraseFile != "" && r.PassCommand != "" {
					add("%s/%s: passphrase_file or passcommand, not both", where, r.Name)
				}
				if strings.HasPrefix(r.Location, "-") {
					add("%s/%s: invalid location", where, r.Name)
				}
				switch r.Check.Mode {
				case "repository", "archives", "both":
				default:
					add("%s/%s: check.mode must be repository, archives or both", where, r.Name)
				}
				if _, _, err := ParseWindow(r.Check.Window); err != nil {
					add("%s/%s: check.window: %v", where, r.Name, err)
				}
				if r.Check.Enabled() && !r.Queried() {
					add("%s/%s: check needs repository access (query: false)", where, r.Name)
				}
			}
		}
	}
	if len(errs) > 0 {
		return errors.New("config:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// Job/Repository lookups.

type JobRef struct {
	Host string
	Job  *Job
}

func (c *Config) Jobs() []JobRef {
	var out []JobRef
	for h := range c.Hosts {
		for j := range c.Hosts[h].Jobs {
			out = append(out, JobRef{Host: c.Hosts[h].Name, Job: &c.Hosts[h].Jobs[j]})
		}
	}
	return out
}

func (c *Config) JobByToken(token string) (JobRef, bool) {
	for _, j := range c.Jobs() {
		if j.Job.PingToken != "" && j.Job.PingToken == token {
			return j, true
		}
	}
	return JobRef{}, false
}

func (c *Config) JobByID(id string) (JobRef, bool) {
	for _, j := range c.Jobs() {
		if j.Job.ID == id {
			return j, true
		}
	}
	return JobRef{}, false
}

func (c *Config) RepoByID(id string) (JobRef, *Repository, bool) {
	for _, j := range c.Jobs() {
		for r := range j.Job.Repositories {
			if j.Job.Repositories[r].ID == id {
				return j, &j.Job.Repositories[r], true
			}
		}
	}
	return JobRef{}, nil, false
}

func (j Job) RestoreEnabled(global bool) bool {
	if j.RestoreTest.Enabled != nil {
		return global && *j.RestoreTest.Enabled
	}
	return global
}

var windowRe = regexp.MustCompile(`^(\d{1,2}):(\d{2})\s*-\s*(\d{1,2}):(\d{2})$`)

// ParseWindow reads "HH:MM-HH:MM" as minutes of the day; empty = whole day.
// A window may span midnight ("22:00-04:00").
func ParseWindow(s string) (from, to int, err error) {
	if strings.TrimSpace(s) == "" {
		return 0, 24 * 60, nil
	}
	m := windowRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, fmt.Errorf("%q: erwartet HH:MM-HH:MM", s)
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	if n(1) > 23 || n(3) > 24 || n(2) > 59 || n(4) > 59 {
		return 0, 0, fmt.Errorf("%q: ungültige Uhrzeit", s)
	}
	return n(1)*60 + n(2), n(3)*60 + n(4), nil
}

// InWindow: is t (in loc) inside the window?
func InWindow(window string, t time.Time, loc *time.Location) bool {
	from, to, err := ParseWindow(window)
	if err != nil {
		return false
	}
	lt := t.In(loc)
	m := lt.Hour()*60 + lt.Minute()
	if from <= to {
		return m >= from && m < to
	}
	return m >= from || m < to // across midnight
}
