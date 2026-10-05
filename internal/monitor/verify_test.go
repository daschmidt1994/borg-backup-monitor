package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

func runWithFiles(t time.Time, files int64) *store.Run {
	r := run(t, time.Minute, store.Success)
	r.Stats = &store.Stats{Files: files}
	return r
}

func TestDrift(t *testing.T) {
	ago := func(h float64) time.Time { return now.Add(-time.Duration(h * float64(time.Hour))) }
	runs := []*store.Run{runWithFiles(ago(96), 1000), runWithFiles(ago(72), 1010), runWithFiles(ago(48), 990), runWithFiles(ago(5), 300)}
	d := computeDrift(runs, 0.3)
	if d == nil || !d.Suspicious || d.Median != 1000 || d.Files != 300 {
		t.Fatalf("drift: %+v", d)
	}
	in := base()
	in.Runs, in.DriftThreshold = runs, 0.3
	in.Snap = snap(ago(0.1), true, tp(ago(4.9)))
	if e := Evaluate(in); e.Status.Level != Warn || !strings.Contains(e.Status.Reasons[len(e.Status.Reasons)-1].Text, "Fehlt ein Volume") {
		t.Fatalf("evaluate drift: %+v", e.Status)
	}
	if computeDrift(runs[:2], 0.3) != nil {
		t.Fatal("drift with too few runs")
	}
	runs[3].Stats.Files = 980
	if d := computeDrift(runs, 0.3); d.Suspicious {
		t.Fatal("normal run flagged")
	}
}

func TestCoverageInEvaluate(t *testing.T) {
	in := base()
	in.Runs = []*store.Run{run(now.Add(-5*time.Hour), time.Minute, store.Success)}
	in.Snap = snap(now.Add(-time.Minute), true, tp(now.Add(-299*time.Minute)))
	in.ExpectedPaths = []string{"source/immich", "source/anon"}
	// not yet checked: info only
	if e := Evaluate(in); e.Status.Level != OK {
		t.Fatalf("pending coverage changed the status: %+v", e.Status)
	}
	in.Snap.Coverage = &store.Coverage{Archive: "a", Paths: []store.CoveragePath{{Path: "source/immich", Present: true, Entries: 3}, {Path: "source/anon", Present: false}}}
	if e := Evaluate(in); e.Status.Level != Err || !strings.Contains(e.Status.Reasons[0].Text+e.Status.Reasons[len(e.Status.Reasons)-1].Text, "source/anon") {
		t.Fatalf("missing source: %+v", e.Status)
	}
	in.Snap.Coverage.Paths[1] = store.CoveragePath{Path: "source/anon", Present: true, Entries: 1, Empty: true}
	if e := Evaluate(in); e.Status.Level != Warn {
		t.Fatalf("empty source: %+v", e.Status)
	}
}

func TestEvaluateCheck(t *testing.T) {
	cfg := config.Check{Schedule: config.Duration(7 * 24 * time.Hour)}
	fin := now.Add(-2 * 24 * time.Hour)
	old := now.Add(-30 * 24 * time.Hour)
	cases := []struct {
		res  []*store.CheckResult
		cfg  config.Check
		want string
	}{
		{nil, config.Check{}, "disabled"},
		{nil, cfg, "never"},
		{[]*store.CheckResult{{State: "passed", StartedAt: fin, FinishedAt: &fin}}, cfg, "passed"},
		{[]*store.CheckResult{{State: "passed", StartedAt: old, FinishedAt: &old}}, cfg, "stale"},
		{[]*store.CheckResult{{State: "passed", StartedAt: old, FinishedAt: &old}, {State: "failed", StartedAt: fin, FinishedAt: &fin}}, cfg, "failed"},
		{[]*store.CheckResult{{State: "running", StartedAt: fin}}, cfg, "running"},
	}
	for i, c := range cases {
		if got := EvaluateCheck(now, c.res, c.cfg); got.State != c.want {
			t.Errorf("case %d: %s, want %s", i, got.State, c.want)
		}
	}
	// a failed check makes the backup status an error
	in := base()
	in.Runs = []*store.Run{run(now.Add(-5*time.Hour), time.Minute, store.Success)}
	in.Snap = snap(now.Add(-time.Minute), true, tp(now.Add(-299*time.Minute)))
	cs := EvaluateCheck(now, cases[4].res, cfg)
	cs.Last.Findings = []store.Finding{{Summary: "Integritätsfehler im Repository"}}
	in.Check = &cs
	if e := Evaluate(in); e.Status.Level != Err {
		t.Fatalf("failed check: %+v", e.Status)
	}
}

func TestWindows(t *testing.T) {
	loc := time.UTC
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, loc) }
	cases := []struct {
		w    string
		t    time.Time
		want bool
	}{
		{"", at(13, 0), true},
		{"01:00-05:00", at(3, 0), true},
		{"01:00-05:00", at(5, 0), false},
		{"22:00-04:00", at(23, 30), true},
		{"22:00-04:00", at(2, 0), true},
		{"22:00-04:00", at(12, 0), false},
		{"kaputt", at(3, 0), false},
	}
	for _, c := range cases {
		if got := config.InWindow(c.w, c.t, loc); got != c.want {
			t.Errorf("%q at %v: %v", c.w, c.t.Format("15:04"), got)
		}
	}
}

type fakeNotifier struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fakeNotifier) Send(_ context.Context, title, msg string, _ Level, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, title)
	return nil
}

type nopBackend struct{ fakeBackend }

func TestNotifyOnlyOnChange(t *testing.T) {
	m := newMon(t)
	m.Backend = nopBackend{}
	n := &fakeNotifier{}
	m.Notifier = n
	clock := time.Now()
	m.Now = func() time.Time { return clock }
	ctx := context.Background()
	m.notifyChanges(ctx) // first sight: records only
	if len(n.msgs) != 0 {
		t.Fatalf("message on first sight: %v", n.msgs)
	}
	_ = m.Ping(token, PingFail, nil, "ERROR Remote: Insufficient free space", "x")
	m.notifyChanges(ctx)
	m.notifyChanges(ctx) // no change → no second message
	if len(n.msgs) != 1 || !strings.HasPrefix(n.msgs[0], "Fehler: nas / system") {
		t.Fatalf("messages: %v", n.msgs)
	}
}
