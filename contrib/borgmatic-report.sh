#!/bin/sh
# borgmatic-report.sh – ruft borgmatic auf und meldet Start, exakten Exit-Code
# und das Log an den Borg Backup Monitor. Optional: borgmatic selbst kann
# über seinen Healthchecks-Hook melden (siehe README); dieses Skript ist nur
# nötig, wenn der genaue Exit-Code gebraucht wird.
#
# Es ersetzt den bisherigen borgmatic-Aufruf in Cron/systemd – der Zeitplan
# bleibt dort, der Monitor startet selbst nie ein Backup.
#
#   BBM_PING_URL=https://monitor.example.com/ping/<token> borgmatic-report.sh --verbosity 1 --stats
set -u
: "${BBM_PING_URL:?BBM_PING_URL setzen (Adresse aus der Job-Ansicht des Monitors)}"
BORGMATIC=${BORGMATIC:-borgmatic}
ping() { curl -fsS -m 30 --retry 3 -A borg-backup-monitor-report -o /dev/null "$@" || echo "borgmatic-report: Meldung an den Monitor fehlgeschlagen" >&2; }

ping -X POST "$BBM_PING_URL/start"
log=$(mktemp) || exit 1
trap 'rm -f "$log"' EXIT INT TERM
"$BORGMATIC" "$@" >"$log" 2>&1
code=$?
cat "$log"
# die letzten 200 KB genügen: dort stehen Ergebnis, Fehler und --stats
tail -c 200000 "$log" | ping -X POST --data-binary @- "$BBM_PING_URL/$code"
exit "$code"
