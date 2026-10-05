package borg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

// fakeBorg writes a shell script that behaves like borg 1.x or 2.x and
// records its arguments and environment.
func fakeBorg(t *testing.T, version string) (bin, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "ARGS: $*" >> "` + record + `"
echo "PASS: $BORG_PASSPHRASE REPO: $BORG_REPO RSH: $BORG_RSH" >> "` + record + `"
case "$1" in
  --version) echo "borg ` + version + `"; exit 0 ;;
esac
for a in "$@"; do case "$a" in --help) echo "usage ... --bypass-lock ... --noacls --noxattrs --noflags"; exit 0 ;; esac; done
case "$BORG_REPO" in
  */slow) sleep 5 ;;
  */wrongpass) echo '{"type": "log_message", "levelname": "ERROR", "msgid": "PassphraseWrong", "message": "passphrase supplied in BORG_PASSPHRASE is incorrect."}' >&2; exit 52 ;;
esac
case "$1" in
  list|repo-list|rlist)
    for a in "$@"; do case "$a" in --json-lines)
      echo '{"type": "d", "mode": "drwxr-xr-x", "path": "etc", "size": 0}'
      echo '{"type": "-", "mode": "-rw-r--r--", "path": "etc/hostname", "size": 4, "mtime": "2026-10-01T10:00:00.000000"}'
      exit 0 ;; esac; done
    if [ "$1" = list ] && [ "` + version + `" != "${version#2}" ]; then :; fi
    echo '{"archives": [{"archive": "a1", "name": "a1", "start": "2026-10-04T02:30:00.000000", "time": "2026-10-04T02:30:00.000000"},
      {"archive": "a2", "name": "a2", "start": "2026-10-05T02:30:00+02:00"}],
      "encryption": {"mode": "repokey-blake2"}, "repository": {"last_modified": "2026-10-05T02:40:00.000000"}}'
    exit 0 ;;
  info) echo '{"archives": [{"stats": {"original_size": 1000, "compressed_size": 600, "deduplicated_size": 50}}], "cache": {"stats": {"total_size": 9000, "unique_csize": 700}}}'; exit 0 ;;
  extract) echo hi > restored.txt; exit 0 ;;
esac
exit 2
`
	bin = filepath.Join(dir, "borg")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

func runner(t *testing.T, bin string) *Runner {
	c, _ := config.Parse([]byte("hosts: []\n"))
	c.DataDir = t.TempDir()
	c.Borg.Binary = bin
	c.Borg.Timeout = config.Duration(2 * time.Second)
	return NewRunner(c)
}

func TestBorg1(t *testing.T) {
	bin, rec := fakeBorg(t, "1.2.8")
	r := runner(t, bin)
	in := r.Detect(context.Background())
	if !in.Supported || in.Major != 1 || !in.BypassLock || strings.Join(in.ListCmd, " ") != "list" {
		t.Fatalf("detect: %+v", in)
	}
	pw := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(pw, []byte("geheim-passwort\n"), 0o600)
	repo := &config.Repository{ID: "r1", Location: "ssh://u@host/./repo", PassphraseFile: pw, SSHKey: "/keys/id ed25519", KnownHosts: "/keys/known_hosts"}
	snap, berr := r.List(context.Background(), repo)
	if berr != nil {
		t.Fatal(berr)
	}
	if snap.ArchiveCount != 2 || snap.Latest.Name != "a2" || snap.Encryption != "repokey-blake2" || !snap.OK {
		t.Fatalf("snapshot: %+v", snap)
	}
	calls, _ := os.ReadFile(rec)
	s := string(calls)
	if !strings.Contains(s, "ARGS: list --json --log-json --bypass-lock") {
		t.Fatalf("list args: %s", s)
	}
	if strings.Contains(strings.Join(argLines(s), "\n"), "geheim") {
		t.Fatal("passphrase appeared in the arguments")
	}
	if !strings.Contains(s, "PASS: geheim-passwort ") || !strings.Contains(s, "-i '/keys/id ed25519' -o IdentitiesOnly=yes") || !strings.Contains(s, "StrictHostKeyChecking=yes") {
		t.Fatalf("env: %s", s)
	}
	// files and extract address the archive as ::NAME after "--"
	items, cut, berr := r.Files(context.Background(), repo, "a2", "etc", 100)
	if berr != nil || cut || len(items) != 2 || items[1].Type != "file" || items[1].Size != 4 {
		t.Fatalf("files: %+v %v %v", items, cut, berr)
	}
	dir := t.TempDir()
	if _, code, err := r.Extract(context.Background(), repo, ExtractSpec{Archive: "a2", Paths: []string{"etc/hostname"}, Dir: dir, Timeout: 5 * time.Second}); err != nil || code != 0 {
		t.Fatalf("extract: %d %v", code, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "restored.txt")); err != nil {
		t.Fatal("extract did not run in the target directory")
	}
	calls, _ = os.ReadFile(rec)
	if !strings.Contains(string(calls), "--noacls --noxattrs --noflags -- ::a2 etc/hostname") {
		t.Fatalf("extract args: %s", calls)
	}
	if _, _, err := r.Extract(context.Background(), repo, ExtractSpec{Archive: "a2", Paths: []string{"../etc/passwd"}, Dir: dir, Timeout: time.Second}); err == nil {
		t.Fatal("path with .. accepted")
	}
	sz, berr := r.Sizes(context.Background(), repo)
	if berr != nil || sz.Original != 1000 || sz.Deduplicated != 50 || sz.AllDeduplicated != 700 {
		t.Fatalf("sizes: %+v %v", sz, berr)
	}
}

func argLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "ARGS:") {
			out = append(out, l)
		}
	}
	return out
}

func TestBorg2(t *testing.T) {
	bin, rec := fakeBorg(t, "2.0.0b14")
	r := runner(t, bin)
	in := r.Detect(context.Background())
	if !in.Supported || in.Major != 2 || in.ListCmd[0] != "repo-list" {
		t.Fatalf("detect: %+v", in)
	}
	repo := &config.Repository{ID: "r2", Location: "/srv/borg2"}
	if _, berr := r.List(context.Background(), repo); berr != nil {
		t.Fatal(berr)
	}
	if _, _, err := r.Extract(context.Background(), repo, ExtractSpec{Archive: "a2", Paths: []string{"etc"}, Dir: t.TempDir(), Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(rec)
	if !strings.Contains(string(calls), "ARGS: repo-list --json") || !strings.Contains(string(calls), "-- a2 etc") {
		t.Fatalf("borg2 args: %s", calls)
	}
}

func TestBorgErrors(t *testing.T) {
	bin, _ := fakeBorg(t, "1.4.0")
	r := runner(t, bin)
	_, berr := r.List(context.Background(), &config.Repository{ID: "x", Location: "/srv/wrongpass"})
	if berr == nil || berr.Finding.Summary != "Passwort des Repositorys falsch" || berr.Code != 52 {
		t.Fatalf("wrong passphrase: %+v", berr)
	}
	start := time.Now()
	_, berr = r.List(context.Background(), &config.Repository{ID: "y", Location: "/srv/slow"})
	if berr == nil || !strings.Contains(berr.Finding.Summary, "Zeitüberschreitung") || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout: %+v after %v", berr, time.Since(start))
	}
	unsupported, _ := fakeBorg(t, "1.0.9")
	if in := runner(t, unsupported).Detect(context.Background()); in.Supported {
		t.Fatal("borg 1.0 accepted")
	}
	_, berr = runner(t, "/does/not/exist").List(context.Background(), &config.Repository{ID: "z", Location: "/x"})
	if berr == nil || berr.Finding.Summary != "Borg nicht verfügbar" {
		t.Fatalf("missing borg: %+v", berr)
	}
}

func TestExitCodesAndSizes(t *testing.T) {
	for code, want := range map[int]store.Result{0: store.Success, 1: store.Warning, 2: store.Failure, 3: store.Failure, 100: store.Warning, 127: store.Warning, 137: store.Failure, -1: store.Failure} {
		if got, _ := ClassifyExit(code); got != want {
			t.Errorf("exit %d: %s, want %s", code, got, want)
		}
	}
	for s, want := range map[string]int64{"512 B": 512, "1.5 kB": 1500, "10.84 GB": 10840000000, "2 GiB": 2 << 30, "1,234.5 MB": 1234500000} {
		if got := ParseBorgSize(s); got != want {
			t.Errorf("%q: %d, want %d", s, got, want)
		}
	}
	if !ValidArchivePath("etc/hosts") || ValidArchivePath("../x") || ValidArchivePath("-rf") || ValidArchivePath("a/../../b") {
		t.Error("ValidArchivePath")
	}
}
