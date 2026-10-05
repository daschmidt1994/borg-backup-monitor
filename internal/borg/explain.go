package borg

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

// ClassifyExit maps a borg/borgmatic exit code to a result.
// Legacy codes: 0 ok, 1 warning, 2 error. Modern codes (Borg ≥1.4 with
// BORG_EXIT_CODES=modern, Borg 2): 3–99 specific errors, 100–127 specific
// warnings. 128+N: ended by signal N.
func ClassifyExit(code int) (store.Result, string) {
	switch {
	case code == 0:
		return store.Success, "Exit-Code 0 – erfolgreich"
	case code == 1:
		return store.Warning, "Exit-Code 1 – mit Warnungen beendet"
	case code >= 100 && code <= 127:
		return store.Warning, "Exit-Code " + strconv.Itoa(code) + " – mit Warnung beendet"
	case code > 128 && code < 160:
		return store.Failure, "Exit-Code " + strconv.Itoa(code) + " – durch Signal " + strconv.Itoa(code-128) + " abgebrochen" + signalName(code-128)
	case code < 0:
		return store.Failure, "kein Exit-Code – Prozess nicht regulär beendet"
	default:
		return store.Failure, "Exit-Code " + strconv.Itoa(code) + " – Fehler"
	}
}

func signalName(n int) string {
	switch n {
	case 9:
		return " (SIGKILL – z. B. Speichermangel/OOM-Killer)"
	case 15:
		return " (SIGTERM – beendet)"
	case 2:
		return " (SIGINT – abgebrochen)"
	}
	return ""
}

type rule struct {
	re      *regexp.Regexp
	msgid   string
	level   string
	summary string
	hint    string
}

var rules = []rule{
	{msgid: "Repository.DoesNotExist", re: regexp.MustCompile(`(?i)repository .* does not exist|Repository not found`), level: "error",
		summary: "Repository nicht gefunden", hint: "Pfad/URL des Repositorys und die Erreichbarkeit des Speicherziels prüfen."},
	{msgid: "Repository.InvalidRepository", re: regexp.MustCompile(`(?i)is not a valid repository`), level: "error",
		summary: "Kein gültiges Borg-Repository am angegebenen Ort", hint: "Location prüfen – zeigt sie auf das Repository selbst?"},
	{msgid: "PassphraseWrong", re: regexp.MustCompile(`(?i)passphrase supplied .* is incorrect|passphrase .* (wrong|incorrect)`), level: "error",
		summary: "Passwort des Repositorys falsch", hint: "passphrase_file / passcommand für dieses Repository prüfen."},
	{msgid: "PasswordRetriesExceeded", re: regexp.MustCompile(`(?i)exceeded the maximum password retries|Enter passphrase`), level: "error",
		summary: "Kein Passwort verfügbar", hint: "passphrase_file oder passcommand für dieses Repository eintragen."},
	{msgid: "LockTimeout", re: regexp.MustCompile(`(?i)Failed to create/acquire the lock|lock.*timeout`), level: "error",
		summary: "Repository gesperrt – vermutlich läuft gerade ein Backup oder ein alter Lock blieb zurück",
		hint:    "Später erneut abfragen. Bleibt es dauerhaft, auf dem Backup-Host mit „borg break-lock“ prüfen (nicht über diese Oberfläche)."},
	{msgid: "LockFailed", re: regexp.MustCompile(`(?i)Failed to create/acquire the lock`), level: "error",
		summary: "Lock konnte nicht angelegt werden", hint: "Schreibrechte/Speicherplatz am Repository prüfen."},
	{msgid: "ConnectionClosed", re: regexp.MustCompile(`(?i)Connection closed by remote host|Remote: ssh:|ssh: connect to host|Connection (refused|timed out|reset)|No route to host|Could not resolve hostname|Name or service not known`), level: "error",
		summary: "SSH-Verbindung zum Repository fehlgeschlagen", hint: "Erreichbarkeit des Servers, Port und Netzwerk prüfen."},
	{msgid: "", re: regexp.MustCompile(`(?i)Host key verification failed|REMOTE HOST IDENTIFICATION HAS CHANGED`), level: "error",
		summary: "SSH-Hostschlüssel unbekannt oder geändert", hint: "known_hosts für dieses Repository prüfen. Geänderte Schlüssel nie ungeprüft übernehmen."},
	{msgid: "", re: regexp.MustCompile(`(?i)Permission denied \((publickey|password)|Too many authentication failures`), level: "error",
		summary: "SSH-Anmeldung abgelehnt", hint: "ssh_key und die Freigabe des Schlüssels auf dem Speicherziel prüfen."},
	{msgid: "Repository.InsufficientFreeSpaceError", re: regexp.MustCompile(`(?i)Insufficient free space|No space left on device`), level: "error",
		summary: "Kein Speicherplatz mehr frei", hint: "Speicherziel bzw. lokale Platte aufräumen oder erweitern."},
	{msgid: "IntegrityError", re: regexp.MustCompile(`(?i)Data integrity error|IntegrityError|checksum mismatch|segment .* corrupt`), level: "error",
		summary: "Integritätsfehler im Repository", hint: "Auf dem Backup-Host „borg check“ ausführen und Ursache (Speicher/Datenträger) klären."},
	{msgid: "Repository.CheckNeeded", re: regexp.MustCompile(`(?i)check needed|Inconsistency detected`), level: "error",
		summary: "Repository meldet Inkonsistenz – Prüfung nötig", hint: "Auf dem Backup-Host „borg check“ ausführen."},
	{msgid: "Cache.RepositoryAccessAborted", re: regexp.MustCompile(`(?i)Repository access aborted`), level: "error",
		summary: "Zugriff abgebrochen (Sicherheitsabfrage von borg)", hint: "Das Repository wurde verschoben oder ist unbekannt; auf dem Backup-Host prüfen."},
	{msgid: "Cache.RepositoryReplay", re: regexp.MustCompile(`(?i)Cache is newer than repository|replay attack`), level: "error",
		summary: "Cache ist neuer als das Repository – möglicher Rollback des Speicherziels", hint: "Unbedingt prüfen, ob das Repository aus einem alten Stand wiederhergestellt wurde."},
	{msgid: "KeyfileNotFoundError", re: regexp.MustCompile(`(?i)No key file for repository|key file .* not found`), level: "error",
		summary: "Schlüsseldatei des Repositorys fehlt", hint: "Bei keyfile-Verschlüsselung muss die Schlüsseldatei im BORG_BASE_DIR liegen."},
	{msgid: "RemoteRepository.RPCServerOutdated", re: regexp.MustCompile(`(?i)server is too old|RPC .* outdated|Borg server is too old`), level: "error",
		summary: "Borg-Version auf dem Server zu alt", hint: "Borg auf dem Speicherziel aktualisieren oder remote_path setzen."},
	{msgid: "PathNotAllowed", re: regexp.MustCompile(`(?i)Repository path not allowed`), level: "error",
		summary: "Pfad auf dem Server nicht erlaubt (restrict-to-path)", hint: "Freigabe des SSH-Schlüssels auf dem Speicherziel prüfen."},
	{msgid: "Archive.DoesNotExist", re: regexp.MustCompile(`(?i)Archive .* does not exist`), level: "error",
		summary: "Archiv nicht gefunden", hint: "Das Archiv wurde inzwischen vermutlich von borgmatic geprunt."},
	{msgid: "", re: regexp.MustCompile(`(?i)Killed|MemoryError|Cannot allocate memory`), level: "error",
		summary: "Speichermangel – Prozess beendet", hint: "Speicher des Hosts bzw. die Grenzen für borg prüfen."},
	{msgid: "", re: regexp.MustCompile(`(?i)file changed while we backed it up`), level: "warning",
		summary: "Datei hat sich während der Sicherung geändert", hint: "Meist harmlos (Logs, Datenbanken). Für Datenbanken einen Dump sichern statt der Live-Dateien."},
	{msgid: "", re: regexp.MustCompile(`(?i)\[Errno 13\] Permission denied|: Permission denied`), level: "warning",
		summary: "Dateien ohne Leserechte übersprungen", hint: "Diese Dateien fehlen im Archiv. Rechte prüfen oder Pfade ausschließen."},
	{msgid: "", re: regexp.MustCompile(`(?i)\[Errno 2\] No such file or directory`), level: "warning",
		summary: "Quellpfad nicht gefunden", hint: "In borgmatic konfigurierter Pfad existiert nicht (mehr)."},
	{msgid: "", re: regexp.MustCompile(`(?i)Include pattern .* never matched`), level: "warning",
		summary: "Pfad/Muster hat nichts gefunden"},
	{msgid: "", re: regexp.MustCompile(`(?i)terminating with warning status`), level: "warning",
		summary: "Borg mit Warnungen beendet"},
	{msgid: "", re: regexp.MustCompile(`(?i)terminating with error status|Error running configuration file|An error occurred`), level: "error",
		summary: "borg/borgmatic mit Fehler beendet"},
	{msgid: "", re: regexp.MustCompile(`(?i)Command .* returned non-zero exit status (\d+)`), level: "error",
		summary: "Ein Befehl ist mit Fehler-Exit-Code beendet worden"},
}

// Explain turns a borg message (and its msgid, if known) into a finding.
func Explain(msgid, text string) store.Finding {
	text = strings.TrimSpace(text)
	for _, r := range rules {
		if (msgid != "" && r.msgid == msgid) || (r.re != nil && r.re.MatchString(text)) {
			return store.Finding{Level: r.level, Summary: r.summary, Hint: r.hint, Detail: clip(text, 4000)}
		}
	}
	return store.Finding{Level: "error", Summary: "Fehler (keine bekannte Ursache erkannt)", Detail: clip(text, 4000)}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + " …"
}

// explainStderr reads borg's --log-json lines (and plain text as fallback).
func explainStderr(stderr []byte) store.Finding {
	var msgid string
	var lines []string
	for _, l := range strings.Split(string(stderr), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		var j struct {
			Type      string `json:"type"`
			Message   string `json:"message"`
			Levelname string `json:"levelname"`
			Msgid     string `json:"msgid"`
		}
		if strings.HasPrefix(l, "{") && json.Unmarshal([]byte(l), &j) == nil {
			if j.Levelname == "ERROR" || j.Levelname == "CRITICAL" || j.Levelname == "WARNING" {
				lines = append(lines, j.Message)
				if j.Msgid != "" && msgid == "" {
					msgid = j.Msgid
				}
			}
			continue
		}
		lines = append(lines, l)
	}
	return Explain(msgid, strings.Join(lines, "\n"))
}

// --- borgmatic run logs (from the ping body) --------------------------------

var (
	levelRe   = regexp.MustCompile(`\b(WARNING|ERROR|CRITICAL)\b`)
	statsRe   = regexp.MustCompile(`(?m)^\s*(This archive|All archives):\s+([\d.,]+\s*[kKMGTP]?i?B)\s+([\d.,]+\s*[kKMGTP]?i?B)\s+([\d.,]+\s*[kKMGTP]?i?B)`)
	archRe    = regexp.MustCompile(`(?m)Archive name:\s*(\S+)`)
	nfilesRe  = regexp.MustCompile(`(?m)Number of files:\s*(\d+)`)
	sizeValRe = regexp.MustCompile(`^([\d.,]+)\s*([kKMGTP]?)(i?)B$`)
)

// ParseBorgSize reads "1.23 GB" / "512 B" / "3.4 GiB".
func ParseBorgSize(s string) int64 {
	m := sizeValRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	if err != nil {
		return 0
	}
	base := 1000.0
	if m[3] == "i" {
		base = 1024
	}
	if m[2] != "" {
		for i := 0; i <= strings.Index("KMGTP", strings.ToUpper(m[2])); i++ {
			f *= base
		}
	}
	return int64(f)
}

// AnalyzeLog finds warnings, errors and --stats in a borgmatic log.
func AnalyzeLog(log string) (findings []store.Finding, hasWarning, hasError bool, stats *store.Stats) {
	seen := map[string]int{}
	for _, l := range strings.Split(log, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		lvl := levelRe.FindString(l)
		var f store.Finding
		matched := false
		for _, r := range rules {
			if r.re != nil && r.re.MatchString(l) {
				f = store.Finding{Level: r.level, Summary: r.summary, Hint: r.hint, Detail: clip(strings.TrimSpace(l), 1000)}
				matched = true
				break
			}
		}
		switch {
		case matched:
		case lvl == "ERROR" || lvl == "CRITICAL":
			f = store.Finding{Level: "error", Summary: "Fehlermeldung im Log", Detail: clip(strings.TrimSpace(l), 1000)}
		case lvl == "WARNING":
			f = store.Finding{Level: "warning", Summary: "Warnung im Log", Detail: clip(strings.TrimSpace(l), 1000)}
		default:
			continue
		}
		if f.Level == "error" {
			hasError = true
		} else {
			hasWarning = true
		}
		// one finding per kind, details of up to 5 lines
		if i, ok := seen[f.Summary]; ok {
			if strings.Count(findings[i].Detail, "\n") < 4 {
				findings[i].Detail += "\n" + f.Detail
			}
			continue
		}
		seen[f.Summary] = len(findings)
		findings = append(findings, f)
	}
	st := &store.Stats{}
	found := false
	for _, m := range statsRe.FindAllStringSubmatch(log, -1) {
		found = true
		o, c, d := ParseBorgSize(m[2]), ParseBorgSize(m[3]), ParseBorgSize(m[4])
		if m[1] == "This archive" {
			st.Original, st.Compressed, st.Deduplicated = o, c, d
		} else {
			st.AllOriginal, st.AllCompressed, st.AllDeduplicated = o, c, d
		}
	}
	if m := archRe.FindStringSubmatch(log); m != nil {
		st.ArchiveName, found = m[1], true
	}
	if m := nfilesRe.FindStringSubmatch(log); m != nil {
		st.Files, _ = strconv.ParseInt(m[1], 10, 64)
		found = true
	}
	if found {
		stats = st
	}
	return findings, hasWarning, hasError, stats
}
