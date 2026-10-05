package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestExampleConfig(t *testing.T) {
	b, err := os.ReadFile("../../config/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// the placeholders are intentionally invalid – replace them like a user would
	s := strings.ReplaceAll(string(b), "$2a$12$ERSETZEN.DURCH.DEN.ERZEUGTEN.HASH.....................", "$2a$12$abcdefghijklmnopqrstuvabcdefghijklmnopqrstuvwxyz12345")
	s = strings.ReplaceAll(s, "ERSETZEN-docker-compose-run-borg-monitor-new-token", "tok-aaaaaaaaaaaaaaaaaaaa")
	s = strings.ReplaceAll(s, "ERSETZEN-zweiter-token-mit-new-token", "tok-bbbbbbbbbbbbbbbbbbbb")
	s = strings.ReplaceAll(s, "ERSETZEN-dritter-token-mit-new-token", "tok-cccccccccccccccccccc")
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Jobs()) != 3 || c.Restore.MaxBytes != 500_000_000 || c.Restore.MemoryMax != 2<<30 {
		t.Fatalf("parsed: %+v", c.Restore)
	}
	j, _ := c.JobByToken("tok-bbbbbbbbbbbbbbbbbbbb")
	if j.Job.Interval.D() != 12*time.Hour || j.Job.RestoreEnabled(true) {
		t.Fatalf("webserver job: %+v", j.Job)
	}
	l, _ := c.JobByToken("tok-cccccccccccccccccccc")
	if l.Job.Repositories[0].Queried() || l.Job.Interval.D() != 24*time.Hour {
		t.Fatal("laptop: query false / default interval")
	}
}

func TestValidation(t *testing.T) {
	bad := []string{
		"hosts:\n  - name: a\n    jobs:\n      - name: j\n        ping_token: short\n        repositories:\n          - name: r\n            location: /x\n",
		"hosts:\n  - name: a\n    jobs:\n      - name: j\n        repositories: []\n",
		"hosts:\n  - name: a\n    jobs:\n      - name: j\n        repositories:\n          - name: r\n            location: -oProxyCommand=x\n",
		"borg:\n  bypass_lock: maybe\n",
		"unknown_key: 1\n",
		"defaults:\n  interval: soon\n",
	}
	for i, y := range bad {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	for s, want := range map[string]time.Duration{"1d": 24 * time.Hour, "2w": 14 * 24 * time.Hour, "90m": 90 * time.Minute, "1.5d": 36 * time.Hour} {
		if d, err := ParseDuration(s); err != nil || d != want {
			t.Errorf("%s: %v %v", s, d, err)
		}
	}
}
