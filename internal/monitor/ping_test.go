package monitor

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

const token = "test-token-0123456789"

func newMon(t *testing.T) *Monitor {
	t.Helper()
	c, err := config.Parse([]byte(`
hosts:
  - name: nas
    jobs:
      - name: system
        ping_token: ` + token + `
        repositories:
          - name: r
            location: /srv/borg
`))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.Open("")
	m := New(c, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m
}

func lastRun(m *Monitor) *store.Run {
	var r *store.Run
	m.Store.Read(func(s *store.State) {
		runs := s.Runs[m.Cfg.Jobs()[0].Job.ID]
		if len(runs) > 0 {
			c := *runs[len(runs)-1]
			r = &c
		}
	})
	return r
}

func TestPingLifecycle(t *testing.T) {
	m := newMon(t)
	clock := time.Date(2026, 10, 5, 2, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return clock }

	if err := m.Ping("wrong-token-xxxxxxxx", PingSuccess, nil, "", "x"); err == nil {
		t.Fatal("unknown token accepted")
	}
	// start → finish with a borg warning in the log
	if err := m.Ping(token, PingStart, nil, "", "healthchecks-ping"); err != nil {
		t.Fatal(err)
	}
	if r := lastRun(m); r.Result != store.Running || r.StartedAt == nil {
		t.Fatalf("after start: %+v", r)
	}
	clock = clock.Add(12 * time.Minute)
	log := "INFO creating archive\nWARNING /var/lib/mysql/ibdata1: file changed while we backed it up\nThis archive:   10.84 GB   8.12 GB   214.60 MB\nAll archives:  402.17 GB  301.55 GB   98.20 GB\n"
	if err := m.Ping(token, PingSuccess, nil, log, "healthchecks-ping"); err != nil {
		t.Fatal(err)
	}
	r := lastRun(m)
	if r.Result != store.Warning || *r.Duration() != 12*time.Minute {
		t.Fatalf("warning run: %+v", r)
	}
	if r.Stats == nil || r.Stats.Original != 10840000000 || r.Stats.Deduplicated != 214600000 || r.Stats.AllDeduplicated != 98200000000 {
		t.Fatalf("stats: %+v", r.Stats)
	}
	if !strings.Contains(r.Findings[0].Summary, "während der Sicherung geändert") {
		t.Fatalf("findings: %+v", r.Findings)
	}

	// fail without a start ping: own run, cause from the log
	clock = clock.Add(24 * time.Hour)
	if err := m.Ping(token, PingFail, nil, "ERROR Remote: Insufficient free space to complete transaction", "healthchecks-ping"); err != nil {
		t.Fatal(err)
	}
	r = lastRun(m)
	if r.Result != store.Failure || r.StartedAt != nil || r.Findings[0].Summary != "Kein Speicherplatz mehr frei" {
		t.Fatalf("failure: %+v", r)
	}

	// wrapper with exit code: 0 ok, 1 warning, 2 error; cause before exit text
	for code, want := range map[int]store.Result{0: store.Success, 1: store.Warning, 2: store.Failure, 107: store.Warning, 137: store.Failure} {
		c := code
		_ = m.Ping(token, PingStart, nil, "", "wrapper")
		if err := m.Ping(token, PingExit, &c, "", "wrapper"); err != nil {
			t.Fatal(err)
		}
		if r := lastRun(m); r.Result != want || *r.ExitCode != code {
			t.Fatalf("exit %d: %+v", code, r)
		}
	}

	// a start while the previous run is still open: the old one failed silently
	_ = m.Ping(token, PingStart, nil, "", "healthchecks-ping")
	_ = m.Ping(token, PingStart, nil, "", "healthchecks-ping")
	var runs []*store.Run
	m.Store.Read(func(s *store.State) { runs = s.Runs[m.Cfg.Jobs()[0].Job.ID] })
	if prev := runs[len(runs)-2]; prev.Result != store.Failure || !strings.Contains(prev.Findings[0].Summary, "ohne Abschlussmeldung") {
		t.Fatalf("abandoned run: %+v", prev)
	}

	// success reported, but errors in the log → warning
	_ = m.Ping(token, PingSuccess, nil, "ERROR hook failed", "healthchecks-ping")
	if r := lastRun(m); r.Result != store.Warning {
		t.Fatalf("success with error lines: %+v", r)
	}
}
