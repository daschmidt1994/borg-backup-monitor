// Package borg reads Borg repositories – read-only, with timeouts and output
// limits, never through a shell. The only writing command is `borg extract`
// for a restore test, and only into a fresh temporary directory.
//
// Supported: Borg 1.1+ (JSON output) and Borg 2.x (repo-list/rlist).
// Read commands use --bypass-lock where the installed borg offers it, so the
// monitor never holds a lock a running backup would have to wait for.
package borg

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

// Info about the installed borg, detected once.
type Info struct {
	Version    string    `json:"version"`
	Major      int       `json:"major"`
	Minor      int       `json:"minor"`
	Supported  bool      `json:"supported"`
	ListCmd    []string  `json:"list_cmd"` // ["list"] (1.x) | ["repo-list"] | ["rlist"]
	BypassLock bool      `json:"bypass_lock"`
	ExtractOpt []string  `json:"extract_options,omitempty"`
	Error      string    `json:"error,omitempty"`
	DetectedAt time.Time `json:"detected_at"`
}

type Runner struct {
	Binary     string
	Timeout    time.Duration
	MaxOutput  int64
	BypassLock string // auto | always | never
	BaseDir    string // BORG_BASE_DIR and HOME for borg/ssh
	Loc        *time.Location

	sem  chan struct{}
	mu   sync.Mutex
	info *Info
}

func NewRunner(c *config.Config) *Runner {
	return &Runner{
		Binary: c.Borg.Binary, Timeout: c.Borg.Timeout.D(), MaxOutput: int64(c.Borg.MaxOutput),
		BypassLock: c.Borg.BypassLock, BaseDir: filepath.Join(c.DataDir, "borg"), Loc: c.Location,
		sem: make(chan struct{}, c.Borg.MaxParallel),
	}
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)(\S*)`)

// Detect finds out which borg is installed and which interface it has.
func (r *Runner) Detect(ctx context.Context) *Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.info != nil && r.info.Error == "" && time.Since(r.info.DetectedAt) < time.Hour {
		return r.info
	}
	in := &Info{DetectedAt: time.Now()}
	r.info = in
	out, _, code, err := r.exec(ctx, nil, nil, "", []string{"--version"}, 1<<16)
	if err != nil || code != 0 {
		in.Error = fmt.Sprintf("%s --version: %v", r.Binary, firstErr(err, fmt.Errorf("exit %d", code)))
		return in
	}
	m := versionRe.FindStringSubmatch(string(out))
	if m == nil {
		in.Error = "unbekannte Ausgabe von borg --version: " + strings.TrimSpace(string(out))
		return in
	}
	in.Version = m[0]
	in.Major, _ = strconv.Atoi(m[1])
	in.Minor, _ = strconv.Atoi(m[2])
	switch {
	case in.Major == 1 && in.Minor >= 1:
		in.Supported, in.ListCmd = true, []string{"list"}
	case in.Major >= 2:
		in.Supported, in.ListCmd = true, []string{"repo-list"}
		if _, _, c, _ := r.exec(ctx, nil, nil, "", []string{"repo-list", "--help"}, 1<<20); c != 0 {
			in.ListCmd = []string{"rlist"} // early 2.0 betas
		}
	default:
		in.Error = "Borg " + in.Version + " wird nicht unterstützt (ab 1.1 mit JSON-Ausgabe)"
		return in
	}
	help, _, _, _ := r.exec(ctx, nil, nil, "", append(append([]string{}, in.ListCmd...), "--help"), 1<<20)
	switch r.BypassLock {
	case "always":
		in.BypassLock = true
	case "auto":
		in.BypassLock = bytes.Contains(help, []byte("--bypass-lock"))
	}
	eh, _, _, _ := r.exec(ctx, nil, nil, "", []string{"extract", "--help"}, 1<<20)
	for _, o := range []string{"--noacls", "--noxattrs", "--noflags"} {
		if bytes.Contains(eh, []byte(o)) {
			in.ExtractOpt = append(in.ExtractOpt, o)
		}
	}
	if !bytes.Contains(eh, []byte("--noflags")) && bytes.Contains(eh, []byte("--nobsdflags")) {
		in.ExtractOpt = append(in.ExtractOpt, "--nobsdflags")
	}
	return in
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// shQuote quotes for BORG_RSH, which borg splits like a shell (shlex).
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

// env builds a minimal environment: only what borg and ssh need. Secrets
// go to the borg process only and are never logged.
func (r *Runner) env(repo *config.Repository) ([]string, error) {
	home := filepath.Join(r.BaseDir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8",
		"BORG_BASE_DIR=" + r.BaseDir,
		"BORG_REPO=" + repo.Location,
		// never answer a question with yes
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=no",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=no",
		"BORG_CHECK_I_KNOW_WHAT_I_AM_DOING=NO",
		"BORG_DELETE_I_KNOW_WHAT_I_AM_DOING=NO",
		"BORG_DISPLAY_PASSPHRASE=no",
	}
	switch {
	case repo.PassphraseFile != "":
		b, err := os.ReadFile(repo.PassphraseFile)
		if err != nil {
			return nil, fmt.Errorf("Passwortdatei nicht lesbar: %w", err)
		}
		env = append(env, "BORG_PASSPHRASE="+strings.TrimRight(string(b), "\r\n"))
	case repo.PassCommand != "":
		env = append(env, "BORG_PASSCOMMAND="+repo.PassCommand)
	default:
		env = append(env, "BORG_PASSPHRASE=") // no interactive prompt
	}
	rsh := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=20", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
	if repo.SSHKey != "" {
		rsh = append(rsh, "-i", shQuote(repo.SSHKey), "-o", "IdentitiesOnly=yes")
	}
	if repo.KnownHosts != "" {
		rsh = append(rsh, "-o", "UserKnownHostsFile="+shQuote(repo.KnownHosts), "-o", "StrictHostKeyChecking=yes")
	}
	env = append(env, "BORG_RSH="+strings.Join(rsh, " "))
	return env, nil
}

// limitWriter stops accepting output beyond max and reports it.
type limitWriter struct {
	buf      bytes.Buffer
	max      int64
	exceeded bool
	cancel   func()
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if int64(w.buf.Len()+len(p)) > w.max {
		w.buf.Write(p[:max(0, int(w.max)-w.buf.Len())])
		w.exceeded = true
		if w.cancel != nil {
			w.cancel()
		}
		return 0, errors.New("output limit exceeded")
	}
	return w.buf.Write(p)
}

var errOutputLimit = errors.New("Ausgabe von borg zu groß (Grenze borg.max_output)")

// exec runs borg with a fixed argument list – no shell – in its own process
// group, so a timeout also ends ssh.
func (r *Runner) exec(ctx context.Context, env []string, prefix []string, dir string, args []string, maxOut int64) (stdout, stderr []byte, code int, err error) {
	return r.execT(ctx, r.Timeout, env, prefix, dir, args, maxOut)
}

func (r *Runner) execT(ctx context.Context, timeout time.Duration, env []string, prefix []string, dir string, args []string, maxOut int64) (stdout, stderr []byte, code int, err error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, nil, -1, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append(append(append([]string{}, prefix...), r.Binary), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	out := &limitWriter{max: maxOut, cancel: cancel}
	errb := &limitWriter{max: 1 << 20}
	cmd.Stdout, cmd.Stderr = out, errb
	err = cmd.Run()
	code = -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	switch {
	case out.exceeded:
		err = errOutputLimit
	case ctx.Err() == context.DeadlineExceeded:
		err = fmt.Errorf("Zeitlimit von %s überschritten", timeout)
	case err != nil && code >= 0:
		err = nil // non-zero exit: the caller looks at code and stderr
	}
	return out.buf.Bytes(), errb.buf.Bytes(), code, err
}

func (r *Runner) common(in *Info, repo *config.Repository) []string {
	a := []string{"--log-json"}
	if in.BypassLock {
		a = append(a, "--bypass-lock")
	}
	if repo.RemotePath != "" {
		a = append(a, "--remote-path", repo.RemotePath)
	}
	return a
}

// archiveArg: borg 1.x addresses an archive as ::NAME (repository from
// BORG_REPO), borg 2 by its name.
func archiveArg(in *Info, name string) string {
	if in.Major == 1 {
		return "::" + name
	}
	return name
}

// Error is a failed borg call, explained.
type Error struct {
	Finding store.Finding
	Code    int
}

func (e *Error) Error() string { return e.Finding.Summary }

func (r *Runner) call(ctx context.Context, repo *config.Repository, cmd []string, maxOut int64) (*Info, []byte, *Error) {
	in, out, _, e := r.callT(ctx, repo, cmd, maxOut, false)
	return in, out, e
}

// callT: with allowCut, output beyond maxOut is cut off instead of failing.
func (r *Runner) callT(ctx context.Context, repo *config.Repository, cmd []string, maxOut int64, allowCut bool) (*Info, []byte, bool, *Error) {
	in := r.Detect(ctx)
	if !in.Supported {
		return in, nil, false, &Error{Finding: store.Finding{Level: "error", Summary: "Borg nicht verfügbar", Detail: in.Error,
			Hint: "borg.binary in der Konfiguration prüfen; unterstützt ist Borg ab 1.1."}}
	}
	env, err := r.env(repo)
	if err != nil {
		return in, nil, false, &Error{Finding: store.Finding{Level: "error", Summary: "Zugangsdaten nicht lesbar", Detail: err.Error()}}
	}
	out, stderr, code, err := r.exec(ctx, env, nil, "", cmd, maxOut)
	if allowCut && errors.Is(err, errOutputLimit) {
		return in, out, true, nil
	}
	if err != nil {
		f := Explain("", err.Error()+"\n"+string(stderr))
		f.Level = "error"
		if strings.Contains(err.Error(), "Zeitlimit") {
			f.Summary = "Zeitüberschreitung bei der Abfrage – Repository nicht erreichbar oder sehr langsam"
		}
		return in, nil, false, &Error{Finding: f, Code: code}
	}
	if code != 0 {
		res, _ := ClassifyExit(code)
		f := explainStderr(stderr)
		if res == store.Warning && len(out) > 0 {
			return in, out, false, nil // warning, but the data is there
		}
		f.Level = "error"
		f.Detail = strings.TrimSpace(fmt.Sprintf("Exit-Code %d\n%s", code, f.Detail))
		return in, nil, false, &Error{Finding: f, Code: code}
	}
	return in, out, false, nil
}

// --- list ---------------------------------------------------------------

type jsonArchive struct {
	Name     string `json:"name"`
	Archive  string `json:"archive"`
	Start    string `json:"start"`
	Time     string `json:"time"`
	End      string `json:"end"`
	Hostname string `json:"hostname"`
}

type jsonList struct {
	Archives   []jsonArchive `json:"archives"`
	Encryption struct {
		Mode string `json:"mode"`
	} `json:"encryption"`
	Repository struct {
		LastModified string `json:"last_modified"`
	} `json:"repository"`
}

// ParseTime reads borg timestamps: Borg 2 with offset, Borg 1.x without
// (local time of the machine running borg – configurable via time_zone).
func ParseTime(s string, loc *time.Location) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	for _, f := range []string{"2006-01-02T15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(f, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// List queries the archives of a repository (manifest only – cheap).
func (r *Runner) List(ctx context.Context, repo *config.Repository) (*store.RepoSnapshot, *Error) {
	t0 := time.Now()
	in := r.Detect(ctx)
	args := append(append(append([]string{}, in.ListCmd...), "--json"), r.common(in, repo)...)
	in, out, berr := r.call(ctx, repo, args, r.MaxOutput)
	snap := &store.RepoSnapshot{RepoID: repo.ID, CheckedAt: time.Now(), BorgVersion: in.Version}
	if berr != nil {
		return snap, berr
	}
	var l jsonList
	if err := json.Unmarshal(out, &l); err != nil {
		return snap, &Error{Finding: store.Finding{Level: "error", Summary: "Antwort von borg nicht lesbar", Detail: err.Error()}}
	}
	list := make([]store.Archive, 0, len(l.Archives))
	for _, a := range l.Archives {
		name := firstNonEmpty(a.Name, a.Archive)
		start, ok := ParseTime(firstNonEmpty(a.Start, a.Time), r.Loc)
		if !ok {
			continue
		}
		ar := store.Archive{Name: name, Start: start, Hostname: a.Hostname}
		if end, ok := ParseTime(a.End, r.Loc); ok {
			ar.End = &end
		}
		list = append(list, ar)
	}
	snap.SetArchives(list)
	snap.OK = true
	snap.Encryption = l.Encryption.Mode
	if t, ok := ParseTime(l.Repository.LastModified, r.Loc); ok {
		snap.LastModified = &t
	}
	snap.QueryDuration = time.Since(t0).Seconds()
	return snap, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- info (sizes; needs the borg cache, so only on request / rarely) -------

type jsonInfo struct {
	Archives []struct {
		Stats struct {
			Original     int64 `json:"original_size"`
			Compressed   int64 `json:"compressed_size"`
			Deduplicated int64 `json:"deduplicated_size"`
		} `json:"stats"`
	} `json:"archives"`
	Cache struct {
		Stats struct {
			TotalSize   int64 `json:"total_size"`
			UniqueCSize int64 `json:"unique_csize"`
			UniqueSize  int64 `json:"unique_size"`
		} `json:"stats"`
	} `json:"cache"`
}

func (r *Runner) Sizes(ctx context.Context, repo *config.Repository) (*store.Sizes, *Error) {
	in := r.Detect(ctx)
	args := append([]string{"info", "--json", "--last", "1"}, r.common(in, repo)...)
	_, out, berr := r.call(ctx, repo, args, 4<<20)
	if berr != nil {
		return nil, berr
	}
	var j jsonInfo
	if err := json.Unmarshal(out, &j); err != nil {
		return nil, &Error{Finding: store.Finding{Level: "error", Summary: "Antwort von borg info nicht lesbar", Detail: err.Error()}}
	}
	s := &store.Sizes{At: time.Now(), Source: "borg info", AllOriginal: j.Cache.Stats.TotalSize,
		AllDeduplicated: firstPositive(j.Cache.Stats.UniqueCSize, j.Cache.Stats.UniqueSize)}
	if len(j.Archives) > 0 {
		st := j.Archives[0].Stats
		s.Original, s.Compressed, s.Deduplicated = st.Original, st.Compressed, st.Deduplicated
	}
	return s, nil
}

func firstPositive(v ...int64) int64 {
	for _, x := range v {
		if x > 0 {
			return x
		}
	}
	return 0
}

// --- files of an archive (for choosing what to restore) --------------------

type Item struct {
	Path       string    `json:"path"`
	Type       string    `json:"type"` // file | dir | symlink | other
	Size       int64     `json:"size"`
	Mode       string    `json:"mode,omitempty"`
	MTime      time.Time `json:"mtime,omitempty"`
	LinkTarget string    `json:"link_target,omitempty"`
}

func itemType(t, mode string) string {
	c := t
	if c == "" && mode != "" {
		c = mode[:1]
	}
	switch c {
	case "-":
		return "file"
	case "d":
		return "dir"
	case "l":
		return "symlink"
	}
	return "other"
}

// ValidArchivePath rejects anything that is not a plain relative path.
func ValidArchivePath(p string) bool {
	if p == "" || len(p) > 4096 || strings.HasPrefix(p, "-") || strings.ContainsAny(p, "\x00\n\r") {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// Files lists at most max items of an archive below prefix.
func (r *Runner) Files(ctx context.Context, repo *config.Repository, archive, prefix string, max int) ([]Item, bool, *Error) {
	if prefix != "" && !ValidArchivePath(prefix) {
		return nil, false, &Error{Finding: store.Finding{Level: "error", Summary: "Ungültiger Pfad"}}
	}
	in := r.Detect(ctx)
	args := append([]string{"list", "--json-lines"}, r.common(in, repo)...)
	args = append(args, "--", archiveArg(in, archive))
	if prefix != "" {
		args = append(args, strings.TrimPrefix(prefix, "/"))
	}
	// read up to max lines; longer listings are cut off
	_, out, truncated, berr := r.callT(ctx, repo, args, int64(max+1)*2048, true)
	if berr != nil {
		return nil, false, berr
	}
	var items []Item
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var j struct {
			Type, Mode, Path, LinkTarget, MTime string
			Size                                int64
		}
		if json.Unmarshal(sc.Bytes(), &j) != nil {
			continue
		}
		it := Item{Path: j.Path, Type: itemType(j.Type, j.Mode), Size: j.Size, Mode: j.Mode, LinkTarget: j.LinkTarget}
		if t, ok := ParseTime(j.MTime, r.Loc); ok {
			it.MTime = t
		}
		items = append(items, it)
		if len(items) >= max {
			truncated = true
			break
		}
	}
	return items, truncated, nil
}

// --- extract (restore test only) -------------------------------------------

// ExtractSpec: everything the extract needs; the caller has created dir.
type ExtractSpec struct {
	Archive   string
	Paths     []string
	Dir       string
	Timeout   time.Duration
	MemoryMax int64
}

// Extract restores the given paths into spec.Dir (the working directory –
// borg extract always writes relative to it and refuses "..").
func (r *Runner) Extract(ctx context.Context, repo *config.Repository, spec ExtractSpec) (stderr string, code int, err error) {
	in := r.Detect(ctx)
	if !in.Supported {
		return "", -1, errors.New(in.Error)
	}
	for _, p := range spec.Paths {
		if !ValidArchivePath(p) {
			return "", -1, fmt.Errorf("ungültiger Pfad %q", p)
		}
	}
	env, err := r.env(repo)
	if err != nil {
		return "", -1, err
	}
	args := append([]string{"extract"}, r.common(in, repo)...)
	args = append(args, in.ExtractOpt...)
	args = append(args, "--", archiveArg(in, spec.Archive))
	for _, p := range spec.Paths {
		args = append(args, strings.TrimPrefix(p, "/"))
	}
	var prefix []string
	if pl, e := exec.LookPath("prlimit"); e == nil && spec.MemoryMax > 0 {
		prefix = append(prefix, pl, "--as="+strconv.FormatInt(spec.MemoryMax, 10), "--")
	}
	if n, e := exec.LookPath("nice"); e == nil {
		prefix = append(prefix, n, "-n", "10")
	}
	_, se, code, err := r.execT(ctx, spec.Timeout, env, prefix, spec.Dir, args, 1<<20)
	return string(se), code, err
}

// HasPrlimit tells whether the memory limit can be enforced.
func HasPrlimit() bool { _, err := exec.LookPath("prlimit"); return err == nil }
