package monitor

import (
	"fmt"
	"strings"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

// Level of a status, worst first when sorted by Rank.
type Level string

const (
	OK      Level = "ok"
	Warn    Level = "warning"
	Err     Level = "error"
	Overdue Level = "overdue"
	Unknown Level = "unknown"
	Info    Level = "info" // explanation only, does not change the status
)

func (l Level) Rank() int {
	switch l {
	case Err:
		return 5
	case Overdue:
		return 4
	case Warn:
		return 3
	case Unknown:
		return 2
	case Info:
		return 1
	}
	return 0
}

func (l Level) Label() string {
	switch l {
	case Err:
		return "Fehler"
	case Overdue:
		return "Überfällig"
	case Warn:
		return "Warnung"
	case Unknown:
		return "Unbekannt"
	}
	return "OK"
}

type Reason struct {
	Level Level  `json:"level"`
	Text  string `json:"text"`
	Hint  string `json:"hint,omitempty"`
}

// Basis: one source the judgement rests on, and how fresh it is.
type Basis struct {
	Source string     `json:"source"`
	Text   string     `json:"text"`
	At     *time.Time `json:"at,omitempty"`
}

type Status struct {
	Level   Level    `json:"level"`
	Label   string   `json:"label"`
	Reasons []Reason `json:"reasons"`
	Basis   []Basis  `json:"basis"`
	// UpdatedAt: newest information the status rests on.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type EvalInput struct {
	Now              time.Time
	Interval         time.Duration
	Tolerance        time.Duration
	RunningWarnAfter time.Duration
	RefreshInterval  time.Duration
	Queried          bool         // repository is listed by the monitor
	PingConfigured   bool         // job has a ping token
	Runs             []*store.Run // oldest first
	Snap             *store.RepoSnapshot
}

type Eval struct {
	Status        Status
	LastAttempt   *store.Run // newest run (finished or running)
	LastFinished  *store.Run
	LastOK        *store.Run
	Running       *store.Run
	LastSuccessAt *time.Time
	SuccessSource string // "run" | "archive" | ""
}

// clockSlack: borgmatic host and monitor clocks may differ a little.
const clockSlack = 10 * time.Minute

func ago(now, t time.Time) string { return HumanDuration(now.Sub(t)) }

// HumanDuration: "3 Std. 5 Min.", "2 Tage 4 Std.", "45 Sek.".
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d Sek.", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d Min.", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 || h >= 10 {
			return fmt.Sprintf("%d Std.", h)
		}
		return fmt.Sprintf("%d Std. %d Min.", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24
		if h == 0 || days >= 7 {
			return fmt.Sprintf("%d Tage", days)
		}
		return fmt.Sprintf("%d Tage %d Std.", days, h)
	}
}

// HumanInterval: "1 Tag", "12 Std.", "1 Woche".
func HumanInterval(d time.Duration) string {
	day := 24 * time.Hour
	switch {
	case d > 0 && d%(7*day) == 0:
		if d == 7*day {
			return "1 Woche"
		}
		return fmt.Sprintf("%d Wochen", d/(7*day))
	case d > 0 && d%day == 0:
		if d == day {
			return "1 Tag"
		}
		return fmt.Sprintf("%d Tage", d/day)
	}
	return HumanDuration(d)
}

func fmtTime(t time.Time) string { return t.Local().Format("02.01.2006 15:04") }

// Evaluate judges one repository of one job. An existing archive alone is
// no proof of a healthy run; a run report alone does not prove the archive
// arrived. Missing or stale information never counts as healthy.
func Evaluate(in EvalInput) Eval {
	var e Eval
	var reasons []Reason
	add := func(l Level, text, hint string) { reasons = append(reasons, Reason{Level: l, Text: text, Hint: hint}) }

	for i := len(in.Runs) - 1; i >= 0; i-- {
		r := in.Runs[i]
		if e.LastAttempt == nil {
			e.LastAttempt = r
		}
		if r.Result == store.Running {
			if e.Running == nil && e.LastFinished == nil {
				e.Running = r
			}
			continue
		}
		if e.LastFinished == nil {
			e.LastFinished = r
		}
		if e.LastOK == nil && (r.Result == store.Success || r.Result == store.Warning) {
			e.LastOK = r
		}
	}
	snap := in.Snap
	var basis []Basis
	var updated time.Time
	touch := func(t time.Time) {
		if t.After(updated) {
			updated = t
		}
	}

	// --- sources ---
	if r := e.LastAttempt; r != nil {
		t := r.ReceivedAt
		txt := "letzte Meldung: " + resultText(r)
		if r.ExitCode != nil {
			txt += fmt.Sprintf(", Exit-Code %d", *r.ExitCode)
		}
		basis = append(basis, Basis{Source: "borgmatic-Meldung (" + r.Source + ")", Text: txt, At: &t})
		touch(t)
	} else if in.PingConfigured {
		basis = append(basis, Basis{Source: "borgmatic-Meldung", Text: "noch keine Meldung empfangen"})
	} else {
		basis = append(basis, Basis{Source: "borgmatic-Meldung", Text: "nicht eingerichtet (kein ping_token)"})
	}
	if in.Queried {
		switch {
		case snap == nil:
			basis = append(basis, Basis{Source: "Repository-Abfrage", Text: "noch nicht abgefragt"})
		case snap.OK:
			t := snap.CheckedAt
			txt := fmt.Sprintf("%d Archive", snap.ArchiveCount)
			if snap.Latest != nil {
				txt += ", neuestes vom " + fmtTime(snap.Latest.Start)
			}
			basis = append(basis, Basis{Source: "Repository-Abfrage (borg list)", Text: txt, At: &t})
			touch(t)
		default:
			t := snap.CheckedAt
			txt := "fehlgeschlagen"
			if snap.LastGoodAt != nil {
				txt += ", letzte erfolgreiche Abfrage am " + fmtTime(*snap.LastGoodAt)
			}
			basis = append(basis, Basis{Source: "Repository-Abfrage (borg list)", Text: txt, At: &t})
			touch(t)
		}
	} else {
		basis = append(basis, Basis{Source: "Repository-Abfrage", Text: "abgeschaltet (query: false)"})
	}
	basis = append(basis, Basis{Source: "Erwartung (Monitoring-Konfiguration)",
		Text: "alle " + HumanInterval(in.Interval) + ", Toleranz " + HumanDuration(in.Tolerance)})

	// --- nothing known ---
	haveArchive := snap != nil && snap.LastGoodAt != nil && snap.Latest != nil
	if len(in.Runs) == 0 && !haveArchive {
		txt := "Noch keine Daten: weder eine borgmatic-Meldung empfangen noch ein Archiv im Repository gefunden."
		hint := ""
		if !in.PingConfigured {
			hint = "ping_token für den Job setzen und in borgmatic als Healthchecks-URL eintragen."
		}
		if in.Queried && snap != nil && !snap.OK && snap.Error != nil {
			add(Err, "Repository nicht abfragbar: "+snap.Error.Summary, snap.Error.Hint)
		} else {
			add(Unknown, txt, hint)
		}
	}

	// --- repository ---
	if in.Queried && snap != nil {
		switch {
		case !snap.OK && snap.Error != nil && len(in.Runs)+boolInt(haveArchive) > 0:
			lvl, txt := Err, "Repository nicht abfragbar: "+snap.Error.Summary
			if e.Running != nil && snap.Error.Summary != "" && containsLock(snap.Error.Summary) {
				lvl, txt = Warn, "Repository gesperrt, während ein Backup läuft – Archivstand nicht bestätigt"
			}
			add(lvl, txt, snap.Error.Hint)
		case snap.OK && in.RefreshInterval > 0 && in.Now.Sub(snap.CheckedAt) > 3*in.RefreshInterval+time.Hour:
			add(Unknown, "Archivdaten veraltet – letzte Abfrage vor "+ago(in.Now, snap.CheckedAt), "Läuft der Monitor-Dienst und ist borg erreichbar?")
		case snap.OK && snap.Latest == nil:
			add(Warn, "Repository erreichbar, enthält aber keine Archive", "")
		}
	} else if in.Queried && snap == nil && len(in.Runs) > 0 {
		add(Unknown, "Repository noch nicht abgefragt – Archivstand unbestätigt", "")
	}
	if !in.Queried {
		add(Info, "Archivstand wird nicht geprüft (query: false) – die Bewertung beruht allein auf den Meldungen von borgmatic.", "")
	}

	// --- runs ---
	if f := e.LastFinished; f != nil {
		switch f.Result {
		case store.Failure:
			txt := "Letzter Lauf fehlgeschlagen (" + fmtTime(finishedOrReceived(f)) + ")"
			hint := ""
			if s := firstFinding(f, "error"); s != nil {
				txt += ": " + s.Summary
				hint = s.Hint
			}
			add(Err, txt, hint)
		case store.Warning:
			txt := "Letzter Lauf mit Warnungen beendet"
			hint := ""
			if s := firstFinding(f, ""); s != nil {
				txt += ": " + s.Summary
				hint = s.Hint
			}
			add(Warn, txt, hint)
		}
	}
	if len(in.Runs) == 0 && haveArchive {
		if in.PingConfigured {
			add(Warn, "Keine borgmatic-Meldung empfangen – ob die Läufe fehlerfrei waren, ist unbekannt. Belegt ist nur ein Archiv vom "+fmtTime(snap.Latest.Start)+".",
				"Healthchecks-Hook in borgmatic eintragen (siehe Job-Details).")
		} else {
			add(Warn, "Lauf-Ergebnisse werden nicht gemeldet – belegt ist nur ein Archiv vom "+fmtTime(snap.Latest.Start)+".",
				"ping_token setzen und borgmatic melden lassen, damit Fehler und Warnungen erkannt werden.")
		}
	}
	if r := e.Running; r != nil && r.StartedAt != nil {
		if in.RunningWarnAfter > 0 && in.Now.Sub(*r.StartedAt) > in.RunningWarnAfter {
			add(Warn, "Backup läuft seit "+ago(in.Now, *r.StartedAt)+" ohne Abschlussmeldung", "Hängt der Lauf, oder wurde er ohne Meldung abgebrochen?")
		} else {
			add(Info, "Ein Backup läuft gerade (seit "+ago(in.Now, *r.StartedAt)+"). Bewertet wird der letzte abgeschlossene Lauf.", "")
		}
	}

	// --- freshness of the last success ---
	switch {
	case e.LastOK != nil:
		t := finishedOrReceived(e.LastOK)
		e.LastSuccessAt, e.SuccessSource = &t, "run"
	case len(in.Runs) == 0 && haveArchive:
		t := snap.Latest.Start
		if snap.Latest.End != nil {
			t = *snap.Latest.End
		}
		e.LastSuccessAt, e.SuccessSource = &t, "archive"
	}
	if e.LastSuccessAt != nil {
		if age := in.Now.Sub(*e.LastSuccessAt); age > in.Interval+in.Tolerance {
			src := "erfolgreiches Backup"
			if e.SuccessSource == "archive" {
				src = "Archiv"
			}
			add(Overdue, fmt.Sprintf("Letztes %s vor %s – erwartet alle %s (+%s Toleranz)", src, HumanDuration(age), HumanInterval(in.Interval), HumanDuration(in.Tolerance)), "")
		}
	} else if len(in.Runs) > 0 && e.LastFinished != nil && e.LastFinished.Result == store.Failure {
		add(Err, "Bisher kein erfolgreicher Lauf gemeldet", "")
	}

	// --- does the reported success show up in the repository? ---
	if ok := e.LastOK; ok != nil && in.Queried && snap != nil && snap.OK && ok.FinishedAt != nil && snap.CheckedAt.After(*ok.FinishedAt) {
		if ok.StartedAt != nil {
			if snap.Latest == nil || snap.Latest.Start.Before(ok.StartedAt.Add(-clockSlack)) {
				add(Warn, "Erfolg gemeldet, aber in diesem Repository kein Archiv aus dem Lauf vom "+fmtTime(*ok.StartedAt)+" gefunden",
					"Schreibt der borgmatic-Job wirklich in dieses Repository? Wurde das Archiv gelöscht?")
			}
		}
	}

	// --- result ---
	level := OK
	for _, r := range reasons {
		if r.Level.Rank() > level.Rank() && r.Level != Info {
			level = r.Level
		}
	}
	if level == OK {
		txt := "Letztes Backup erfolgreich und aktuell"
		if in.Queried && snap != nil && snap.OK {
			txt += ", im Repository bestätigt"
		}
		reasons = append([]Reason{{Level: OK, Text: txt + "."}}, reasons...)
	}
	e.Status = Status{Level: level, Label: level.Label(), Reasons: reasons, Basis: basis}
	if !updated.IsZero() {
		e.Status.UpdatedAt = &updated
	}
	return e
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func containsLock(s string) bool {
	return strings.Contains(s, "gesperrt") || strings.Contains(s, "Lock")
}

func finishedOrReceived(r *store.Run) time.Time {
	if r.FinishedAt != nil {
		return *r.FinishedAt
	}
	return r.ReceivedAt
}

func firstFinding(r *store.Run, level string) *store.Finding {
	for i := range r.Findings {
		if level == "" || r.Findings[i].Level == level {
			return &r.Findings[i]
		}
	}
	if len(r.Findings) > 0 {
		return &r.Findings[0]
	}
	return nil
}

func resultText(r *store.Run) string {
	switch r.Result {
	case store.Running:
		return "läuft"
	case store.Success:
		return "erfolgreich"
	case store.Warning:
		return "mit Warnungen"
	case store.Failure:
		return "fehlgeschlagen"
	}
	return string(r.Result)
}

// --- restore tests (judged separately from backup success) ---------------

type RestoreStatus struct {
	State        string             `json:"state"` // passed | passed-unverified | failed | stale | never | running | disabled
	Label        string             `json:"label"`
	Text         string             `json:"text"`
	Last         *store.RestoreTest `json:"last,omitempty"`
	LastPassedAt *time.Time         `json:"last_passed_at,omitempty"`
}

func EvaluateRestore(now time.Time, tests []*store.RestoreTest, maxAge time.Duration, enabled bool) RestoreStatus {
	var last, lastPassed *store.RestoreTest
	for i := len(tests) - 1; i >= 0; i-- {
		t := tests[i]
		if last == nil {
			last = t
		}
		if lastPassed == nil && (t.State == "passed" || t.State == "passed-unverified") {
			lastPassed = t
		}
	}
	rs := RestoreStatus{Last: last}
	if lastPassed != nil && lastPassed.FinishedAt != nil {
		rs.LastPassedAt = lastPassed.FinishedAt
	}
	switch {
	case last == nil && !enabled:
		rs.State, rs.Label, rs.Text = "disabled", "nicht eingerichtet", "Restore-Tests sind für diesen Job abgeschaltet."
	case last == nil:
		rs.State, rs.Label, rs.Text = "never", "nie getestet", "Eine Wiederherstellung wurde noch nicht getestet."
	case last.State == "running":
		rs.State, rs.Label, rs.Text = "running", "läuft", "Restore-Test läuft seit "+fmtTime(last.StartedAt)+"."
	case last.State == "failed":
		rs.State, rs.Label, rs.Text = "failed", "fehlgeschlagen", "Letzter Restore-Test am "+fmtTime(last.StartedAt)+" fehlgeschlagen."
	case last.FinishedAt != nil && now.Sub(*last.FinishedAt) > maxAge:
		rs.State, rs.Label, rs.Text = "stale", "veraltet", "Letzter erfolgreicher Test vor "+ago(now, *last.FinishedAt)+" – älter als "+HumanDuration(maxAge)+"."
	case last.State == "passed-unverified":
		rs.State, rs.Label, rs.Text = "passed-unverified", "bestanden (ohne Referenz)", "Wiederherstellung erfolgreich, Inhalt aber nicht gegen eine unabhängige Referenz geprüft."
	default:
		rs.State, rs.Label, rs.Text = "passed", "bestanden", "Stichprobe wiederhergestellt und gegen Referenz geprüft."
	}
	return rs
}
