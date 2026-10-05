package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/borg"
	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/monitor"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

// fake: an archive with a normal file, a symlink to /etc/passwd and a
// set-uid executable; "big" writes more than the limit.
type fake struct{ big bool }

func (fake) Detect(context.Context) *borg.Info { return &borg.Info{Supported: true, Major: 1} }
func (fake) List(context.Context, *config.Repository) (*store.RepoSnapshot, *borg.Error) {
	return &store.RepoSnapshot{OK: true}, nil
}
func (fake) Sizes(context.Context, *config.Repository) (*store.Sizes, *borg.Error) { return nil, nil }

var items = []borg.Item{
	{Path: "etc", Type: "dir"},
	{Path: "etc/hostname", Type: "file", Size: 4},
	{Path: "etc/evil", Type: "symlink", LinkTarget: "/etc/passwd"},
	{Path: "etc/tool", Type: "file", Size: 3},
	{Path: "big.bin", Type: "file", Size: 10},
}

func (f fake) Files(_ context.Context, _ *config.Repository, _, prefix string, max int) ([]borg.Item, bool, *borg.Error) {
	var out []borg.Item
	for _, it := range items {
		if it.Path == prefix || strings.HasPrefix(it.Path, prefix+"/") {
			out = append(out, it)
		}
	}
	return out, false, nil
}

func (f fake) Extract(ctx context.Context, _ *config.Repository, spec borg.ExtractSpec) (string, int, error) {
	if f.big {
		fh, _ := os.Create(filepath.Join(spec.Dir, "big.bin"))
		defer fh.Close()
		chunk := make([]byte, 1<<20)
		for {
			select {
			case <-ctx.Done():
				return "", 143, nil
			default:
				fh.Write(chunk)
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	os.MkdirAll(filepath.Join(spec.Dir, "etc"), 0o755)
	os.WriteFile(filepath.Join(spec.Dir, "etc/hostname"), []byte("nas\n"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(spec.Dir, "etc/evil"))
	os.WriteFile(filepath.Join(spec.Dir, "etc/tool"), []byte("#!x"), 0o4755)
	return "", 0, nil
}

func setup(t *testing.T, b borg.Backend, refs string) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	refFile := ""
	if refs != "" {
		refFile = filepath.Join(dir, "sums.txt")
		os.WriteFile(refFile, []byte(refs), 0o600)
	}
	c, err := config.Parse([]byte(`
restore:
  enabled: true
  max_bytes: 3MB
  timeout: 20s
hosts:
  - name: h
    jobs:
      - name: j
        repositories:
          - name: r
            location: /srv/borg
        restore_test:
          reference_checksums: "` + refFile + `"
`))
	if err != nil {
		t.Fatal(err)
	}
	c.DataDir = dir
	c.Restore.BaseDir = filepath.Join(dir, "restore")
	st, _ := store.Open("")
	mon := monitor.New(c, st, b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return New(c, st, b, mon), c.Jobs()[0].Job.Repositories[0].ID
}

func wait(t *testing.T, m *Manager, id string) *store.RestoreTest {
	t.Helper()
	for i := 0; i < 300; i++ {
		var got *store.RestoreTest
		m.Store.Read(func(s *store.State) {
			for _, x := range s.RestoreTests {
				if x.ID == id && x.State != "running" {
					c := *x
					got = &c
				}
			}
		})
		if got != nil {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("restore test did not finish")
	return nil
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestRestorePassesSafely(t *testing.T) {
	m, repo := setup(t, fake{}, sha("nas\n")+"  /etc/hostname\n")
	if _, err := m.PlanTest(context.Background(), repo, "../x", []string{"etc"}); err == nil {
		t.Fatal("invalid archive name accepted")
	}
	if _, err := m.PlanTest(context.Background(), repo, "a1", []string{"../../etc"}); err == nil {
		t.Fatal("path traversal accepted")
	}
	p, err := m.PlanTest(context.Background(), repo, "a1", []string{"etc"})
	if err != nil || !p.OK || p.Files != 2 || p.Bytes != 7 {
		t.Fatalf("plan: %+v %v", p, err)
	}
	if _, err := m.Start("unknown", "u"); err == nil {
		t.Fatal("unknown plan started")
	}
	rt, err := m.Start(p.ID, "anna")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rt.Target, m.Cfg.Restore.BaseDir) {
		t.Fatalf("target outside base: %s", rt.Target)
	}
	res := wait(t, m, rt.ID)
	if res.State != "passed" || res.Verified != 1 || !res.Removed {
		t.Fatalf("result: %+v", res)
	}
	if _, err := os.Stat(rt.Target); !os.IsNotExist(err) {
		t.Fatal("test directory not removed")
	}
	for _, f := range res.Files {
		if f.Path == "etc/evil" && (!f.Restored || f.SHA256 != "") {
			t.Fatalf("symlink followed or missing: %+v", f)
		}
		if f.Path == "etc/hostname" && f.Reference != "match" {
			t.Fatalf("reference: %+v", f)
		}
	}
	if !strings.Contains(res.Findings[len(res.Findings)-1].Summary, "garantiert nicht") {
		t.Fatal("disclaimer missing")
	}
	// a plan is used once
	if _, err := m.Start(p.ID, "anna"); err == nil {
		t.Fatal("plan reused")
	}
}

func TestRestoreStripsExecAndSetuid(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tool"), []byte("#!x"), 0o4755)
	os.Symlink("/etc/passwd", filepath.Join(dir, "evil"))
	files, _, err := inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "tool"))
	if fi.Mode()&0o111 != 0 || fi.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("mode still executable: %v", fi.Mode())
	}
	if files["evil"].typ != "symlink" || files["evil"].sha != "" {
		t.Fatalf("symlink: %+v", files["evil"])
	}
}

func TestRestoreReferenceMismatchFails(t *testing.T) {
	m, repo := setup(t, fake{}, sha("other\n")+"  etc/hostname\n")
	p, _ := m.PlanTest(context.Background(), repo, "a1", []string{"etc/hostname"})
	rt, err := m.Start(p.ID, "anna")
	if err != nil {
		t.Fatal(err)
	}
	if res := wait(t, m, rt.ID); res.State != "failed" {
		t.Fatalf("mismatch not failed: %+v", res)
	}
}

func TestRestoreUnverifiedWithoutReference(t *testing.T) {
	m, repo := setup(t, fake{}, "")
	p, _ := m.PlanTest(context.Background(), repo, "a1", []string{"etc/hostname"})
	rt, _ := m.Start(p.ID, "anna")
	if res := wait(t, m, rt.ID); res.State != "passed-unverified" {
		t.Fatalf("state: %+v", res)
	}
}

func TestRestoreSizeLimit(t *testing.T) {
	m, repo := setup(t, fake{big: true}, "")
	p, _ := m.PlanTest(context.Background(), repo, "a1", []string{"big.bin"})
	rt, err := m.Start(p.ID, "anna")
	if err != nil {
		t.Fatal(err)
	}
	res := wait(t, m, rt.ID)
	if res.State != "failed" || !strings.Contains(res.Findings[0].Summary, "Größenlimit") {
		t.Fatalf("size limit: %+v", res)
	}
	if _, err := os.Stat(rt.Target); !os.IsNotExist(err) {
		t.Fatal("oversized directory kept")
	}
}

func TestPlanRejectsTooLarge(t *testing.T) {
	m, repo := setup(t, fake{}, "")
	m.Cfg.Restore.MaxBytes = 5
	p, err := m.PlanTest(context.Background(), repo, "a1", []string{"etc"})
	if err != nil || p.OK || len(p.Problems) == 0 {
		t.Fatalf("plan should be blocked: %+v %v", p, err)
	}
	if _, err := m.Start(p.ID, "x"); err == nil {
		t.Fatal("blocked plan started")
	}
}
