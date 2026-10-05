package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/demo"
	"github.com/daschmidt1994/borg-backup-monitor/internal/monitor"
	"github.com/daschmidt1994/borg-backup-monitor/internal/restore"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	h, _ := HashPassword("richtig-langes-passwort")
	c, err := config.Parse([]byte(`
auth:
  users:
    - name: anna
      password_hash: "` + h + `"
hosts:
  - name: nas
    jobs:
      - name: system
        ping_token: ping-token-0123456789
        repositories:
          - name: r
            location: /srv/borg
`))
	if err != nil {
		t.Fatal(err)
	}
	c.DataDir = t.TempDir()
	st, _ := store.Open("")
	b := demo.Seed(demo.Config(t.TempDir()), st, time.Now()) // any backend works for these tests
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mon := monitor.New(c, st, b, log)
	srv, err := New(c, mon, restore.New(c, st, b, mon), log, false)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(srv.Handler())
}

func do(t *testing.T, c *http.Client, method, url, body string, csrf bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if csrf {
		req.Header.Set(csrfHeader, csrfValue)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAuthCSRFAndPing(t *testing.T) {
	ts := testServer(t)
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	if r := do(t, c, "GET", ts.URL+"/api/overview", "", false); r.StatusCode != 401 {
		t.Fatalf("overview without login: %d", r.StatusCode)
	}
	if r := do(t, c, "POST", ts.URL+"/api/login", `{"user":"anna","password":"richtig-langes-passwort"}`, false); r.StatusCode != 403 {
		t.Fatalf("login without CSRF header: %d", r.StatusCode)
	}
	if r := do(t, c, "POST", ts.URL+"/api/login", `{"user":"anna","password":"falsch"}`, true); r.StatusCode != 401 {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	r := do(t, c, "POST", ts.URL+"/api/login", `{"user":"anna","password":"richtig-langes-passwort"}`, true)
	if r.StatusCode != 200 {
		t.Fatalf("login: %d", r.StatusCode)
	}
	ck := r.Header.Get("Set-Cookie")
	if !strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Strict") {
		t.Fatalf("cookie flags: %s", ck)
	}
	if r := do(t, c, "GET", ts.URL+"/api/overview", "", false); r.StatusCode != 200 {
		t.Fatalf("overview: %d", r.StatusCode)
	}
	// state-changing request without the CSRF header
	if r := do(t, c, "POST", ts.URL+"/api/restore-tests/plan", `{}`, false); r.StatusCode != 403 {
		t.Fatalf("POST without CSRF header: %d", r.StatusCode)
	}
	// ping: token instead of login; unknown token 404
	if r := do(t, &http.Client{}, "POST", ts.URL+"/ping/ping-token-0123456789/start", "", false); r.StatusCode != 200 {
		t.Fatalf("ping start: %d", r.StatusCode)
	}
	if r := do(t, &http.Client{}, "GET", ts.URL+"/ping/ping-token-0123456789/1", "", false); r.StatusCode != 200 {
		t.Fatalf("ping exit code: %d", r.StatusCode)
	}
	if r := do(t, &http.Client{}, "POST", ts.URL+"/ping/nope-nope-nope-nope/fail", "", false); r.StatusCode != 404 {
		t.Fatalf("unknown token: %d", r.StatusCode)
	}
	if r := do(t, &http.Client{}, "POST", ts.URL+"/ping/ping-token-0123456789/rm-rf", "", false); r.StatusCode != 404 {
		t.Fatalf("bogus ping kind: %d", r.StatusCode)
	}
	// security headers, static files
	r = do(t, c, "GET", ts.URL+"/", "", false)
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || r.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers: %v", r.Header)
	}
	// logout ends the session
	do(t, c, "POST", ts.URL+"/api/logout", "", true)
	if r := do(t, c, "GET", ts.URL+"/api/overview", "", false); r.StatusCode != 401 {
		t.Fatalf("after logout: %d", r.StatusCode)
	}
}

func TestNoDangerousEndpoints(t *testing.T) {
	ts := testServer(t)
	defer ts.Close()
	for _, p := range []string{"/api/prune", "/api/delete", "/api/compact", "/api/check", "/api/exec", "/api/command", "/api/backup"} {
		if r := do(t, &http.Client{}, "POST", ts.URL+p, "", true); r.StatusCode != 404 && r.StatusCode != 405 {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
}
