package monitor

import (
	"strings"
	"testing"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func tp(t time.Time) *time.Time { return &t }

func run(start time.Time, dur time.Duration, res store.Result, findings ...store.Finding) *store.Run {
	r := &store.Run{ID: start.String(), StartedAt: tp(start), Result: res, Source: "healthchecks-ping", ReceivedAt: start.Add(dur), Findings: findings}
	if res != store.Running {
		r.FinishedAt = tp(start.Add(dur))
	} else {
		r.ReceivedAt = start
	}
	return r
}

func snap(checked time.Time, ok bool, latest *time.Time) *store.RepoSnapshot {
	s := &store.RepoSnapshot{CheckedAt: checked, OK: ok}
	if ok {
		s.LastGoodAt = tp(checked)
	}
	if latest != nil {
		s.SetArchives([]store.Archive{{Name: "a", Start: *latest}})
		if !ok {
			s.LastGoodAt = tp(checked.Add(-24 * time.Hour))
		}
	}
	if !ok {
		s.Error = &store.Finding{Level: "error", Summary: "SSH-Verbindung zum Repository fehlgeschlagen"}
	}
	return s
}

func base() EvalInput {
	return EvalInput{Now: now, Interval: 24 * time.Hour, Tolerance: 6 * time.Hour, RunningWarnAfter: 8 * time.Hour,
		RefreshInterval: 15 * time.Minute, Queried: true, PingConfigured: true}
}

func TestEvaluate(t *testing.T) {
	ago := func(h float64) time.Time { return now.Add(-time.Duration(h * float64(time.Hour))) }
	cases := []struct {
		name   string
		mod    func(*EvalInput)
		want   Level
		reason string // part of a reason text
	}{
		{"nothing known", func(in *EvalInput) {}, Unknown, "Noch keine Daten"},
		{"healthy", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(10), 10*time.Minute, store.Success)}
			in.Snap = snap(ago(0.1), true, tp(ago(9.95)))
		}, OK, "im Repository bestätigt"},
		{"last run failed", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(30), time.Hour, store.Success), run(ago(5), time.Minute, store.Failure,
				store.Finding{Level: "error", Summary: "Kein Speicherplatz mehr frei"})}
			in.Snap = snap(ago(0.1), true, tp(ago(30)))
		}, Err, "Kein Speicherplatz"},
		{"warnings", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(5), time.Minute, store.Warning, store.Finding{Level: "warning", Summary: "Datei hat sich geändert"})}
			in.Snap = snap(ago(0.1), true, tp(ago(5)))
		}, Warn, "mit Warnungen"},
		{"overdue", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(40), time.Minute, store.Success)}
			in.Snap = snap(ago(0.1), true, tp(ago(40)))
		}, Overdue, "erwartet alle 1 Tag"},
		{"archive alone is no proof", func(in *EvalInput) {
			in.Snap = snap(ago(0.1), true, tp(ago(3)))
		}, Warn, "Keine borgmatic-Meldung"},
		{"archive alone, old", func(in *EvalInput) {
			in.Snap = snap(ago(0.1), true, tp(ago(50)))
		}, Overdue, "Letztes Archiv"},
		{"unreachable repository is never healthy", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(5), time.Minute, store.Success)}
			in.Snap = snap(ago(0.1), false, tp(ago(5)))
		}, Err, "nicht abfragbar"},
		{"reported success, no archive in repository", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(5), time.Minute, store.Success)}
			in.Snap = snap(ago(0.1), true, tp(ago(29)))
		}, Warn, "kein Archiv aus dem Lauf"},
		{"stale repository data", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(5), time.Minute, store.Success)}
			in.Snap = snap(ago(3), true, tp(ago(5)))
		}, Unknown, "Archivdaten veraltet"},
		{"running for too long", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(20), time.Minute, store.Success), run(ago(10), 0, store.Running)}
			in.Snap = snap(ago(0.1), true, tp(ago(20)))
		}, Warn, "ohne Abschlussmeldung"},
		{"running normally keeps the last result", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(20), time.Minute, store.Success), run(ago(1), 0, store.Running)}
			in.Snap = snap(ago(0.1), true, tp(ago(20)))
		}, OK, "läuft gerade"},
		{"no repository query: runs decide, with a note", func(in *EvalInput) {
			in.Queried = false
			in.Runs = []*store.Run{run(ago(5), time.Minute, store.Success)}
		}, OK, "query: false"},
		{"only failures ever", func(in *EvalInput) {
			in.Runs = []*store.Run{run(ago(5), time.Minute, store.Failure)}
			in.Snap = snap(ago(0.1), true, nil)
		}, Err, "kein erfolgreicher Lauf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base()
			c.mod(&in)
			e := Evaluate(in)
			var texts []string
			for _, r := range e.Status.Reasons {
				texts = append(texts, r.Text)
			}
			all := strings.Join(texts, " | ")
			if e.Status.Level != c.want {
				t.Fatalf("level %s, want %s – %s", e.Status.Level, c.want, all)
			}
			if !strings.Contains(all, c.reason) {
				t.Fatalf("reasons lack %q: %s", c.reason, all)
			}
			if len(e.Status.Basis) < 3 {
				t.Fatalf("basis missing: %+v", e.Status.Basis)
			}
		})
	}
}

func TestEvaluateRestore(t *testing.T) {
	old := now.Add(-40 * 24 * time.Hour)
	recent := now.Add(-2 * 24 * time.Hour)
	cases := []struct {
		tests   []*store.RestoreTest
		enabled bool
		want    string
	}{
		{nil, true, "never"},
		{nil, false, "disabled"},
		{[]*store.RestoreTest{{State: "passed", FinishedAt: &recent}}, true, "passed"},
		{[]*store.RestoreTest{{State: "passed", FinishedAt: &old}}, true, "stale"},
		{[]*store.RestoreTest{{State: "passed-unverified", FinishedAt: &recent}}, true, "passed-unverified"},
		{[]*store.RestoreTest{{State: "passed", FinishedAt: &old}, {State: "failed", FinishedAt: &recent}}, true, "failed"},
		{[]*store.RestoreTest{{State: "running", StartedAt: now}}, true, "running"},
	}
	for i, c := range cases {
		if got := EvaluateRestore(now, c.tests, 30*24*time.Hour, c.enabled); got.State != c.want {
			t.Errorf("case %d: %s, want %s", i, got.State, c.want)
		}
	}
}

func TestHumanInterval(t *testing.T) {
	for d, want := range map[time.Duration]string{24 * time.Hour: "1 Tag", 48 * time.Hour: "2 Tage", 168 * time.Hour: "1 Woche", 12 * time.Hour: "12 Std."} {
		if got := HumanInterval(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
