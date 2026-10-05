// Package web serves the dashboard, its JSON API and the ping endpoint for
// borgmatic. There is no endpoint that creates, prunes, compacts, deletes or
// changes backups, and no free-form command input.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/monitor"
	"github.com/daschmidt1994/borg-backup-monitor/internal/restore"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

//go:embed static
var static embed.FS

const (
	cookieName   = "bbm_session"
	csrfHeader   = "X-Requested-With"
	csrfValue    = "borg-monitor"
	maxPingBody  = 1 << 20
	maxJSONBody  = 64 << 10
	DemoUser     = "demo"
	DemoPassword = "demo"
)

type Server struct {
	Cfg     *config.Config
	Mon     *monitor.Monitor
	Restore *restore.Manager
	Log     *slog.Logger
	Demo    bool

	key   []byte
	users map[string]string // name → bcrypt hash
	dummy string

	limMu   sync.Mutex
	attempt map[string][]time.Time
}

func New(cfg *config.Config, mon *monitor.Monitor, rm *restore.Manager, log *slog.Logger, demo bool) (*Server, error) {
	s := &Server{Cfg: cfg, Mon: mon, Restore: rm, Log: log, Demo: demo, users: map[string]string{}, attempt: map[string][]time.Time{}}
	for _, u := range cfg.Auth.Users {
		s.users[u.Name] = u.PasswordHash
	}
	if demo {
		h, _ := bcrypt.GenerateFromPassword([]byte(DemoPassword), bcrypt.MinCost)
		s.users[DemoUser] = string(h)
	}
	d, _ := bcrypt.GenerateFromPassword([]byte(NewToken()), 12)
	s.dummy = string(d)
	if len(s.users) == 0 {
		return nil, errors.New("kein Benutzer konfiguriert (auth.users) – Hash erzeugen mit: borg-monitor -hash-password")
	}
	key, err := sessionKey(cfg.DataDir, demo)
	if err != nil {
		return nil, err
	}
	s.key = key
	return s, nil
}

// sessionKey is kept in the data directory so sessions survive restarts.
func sessionKey(dir string, demo bool) ([]byte, error) {
	k := make([]byte, 32)
	if demo {
		_, err := rand.Read(k)
		return k, err
	}
	p := filepath.Join(dir, "session.key")
	if b, err := os.ReadFile(p); err == nil && len(b) == 64 {
		return hex.DecodeString(string(b))
	}
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return k, os.WriteFile(p, []byte(hex.EncodeToString(k)), 0o600)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	files := http.FileServer(http.FS(sub))
	mux.Handle("GET /", files)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	for _, m := range []string{"GET", "POST", "PUT"} { // GET also answers HEAD
		mux.HandleFunc(m+" /ping/{token}", s.ping)
		mux.HandleFunc(m+" /ping/{token}/{kind}", s.ping)
	}

	mux.HandleFunc("GET /api/public", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"demo": s.Demo})
	})
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.auth(s.me))
	mux.HandleFunc("GET /api/overview", s.auth(s.overview))
	mux.HandleFunc("GET /api/repos/{id}", s.auth(s.detail))
	mux.HandleFunc("POST /api/repos/{id}/refresh", s.auth(s.refresh))
	mux.HandleFunc("GET /api/repos/{id}/files", s.auth(s.files))
	mux.HandleFunc("POST /api/repos/{id}/check", s.auth(s.check))
	mux.HandleFunc("GET /api/runs/{id}/log", s.auth(s.runLog))
	mux.HandleFunc("POST /api/restore-tests/plan", s.auth(s.planRestore))
	mux.HandleFunc("POST /api/restore-tests", s.auth(s.startRestore))
	mux.HandleFunc("GET /api/restore-tests/{id}", s.auth(s.restoreTest))
	return s.headers(mux)
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		path := r.URL.Path
		if strings.HasPrefix(path, "/ping/") {
			path = "/ping/***" // the token is a secret
		}
		if path != "/healthz" {
			s.Log.Debug("request", "method", r.Method, "path", path, "ms", time.Since(start).Milliseconds())
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// --- sessions ----------------------------------------------------------------

func (s *Server) sign(user string, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(user)) + "." + strconv.FormatInt(exp.Unix(), 10)
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) verify(tok string) (string, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", false
	}
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[2])) != 1 {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	u, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	if _, ok := s.users[string(u)]; !ok {
		return "", false
	}
	return string(u), true
}

type ctxKey struct{}

func userOf(r *http.Request) string { u, _ := r.Context().Value(ctxKey{}).(string); return u }

// auth: valid session; state-changing requests also need the CSRF header
// (with SameSite=Strict cookies a cross-site form can neither set it nor send the cookie).
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		user, ok := "", false
		if err == nil {
			user, ok = s.verify(c.Value)
		}
		if !ok {
			fail(w, http.StatusUnauthorized, "Bitte anmelden")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != csrfValue {
			fail(w, http.StatusForbidden, "Anfrage abgelehnt (CSRF-Schutz)")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tooMany: at most 10 login attempts per 10 minutes and address.
func (s *Server) tooMany(ip string) bool {
	s.limMu.Lock()
	defer s.limMu.Unlock()
	now := time.Now()
	var keep []time.Time
	for _, t := range s.attempt[ip] {
		if now.Sub(t) < 10*time.Minute {
			keep = append(keep, t)
		}
	}
	s.attempt[ip] = append(keep, now)
	return len(keep) >= 10
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) != csrfValue {
		fail(w, http.StatusForbidden, "Anfrage abgelehnt (CSRF-Schutz)")
		return
	}
	if s.tooMany(clientIP(r)) {
		fail(w, http.StatusTooManyRequests, "Zu viele Anmeldeversuche – bitte später erneut versuchen")
		return
	}
	var in struct{ User, Password string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "ungültige Anfrage")
		return
	}
	hash, ok := s.users[in.User]
	if !ok {
		hash = s.dummy // same bcrypt work for unknown users
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil || !ok {
		s.Log.Info("login failed", "user", in.User, "ip", clientIP(r))
		fail(w, http.StatusUnauthorized, "Benutzername oder Passwort falsch")
		return
	}
	exp := time.Now().Add(time.Duration(s.Cfg.Auth.SessionHours) * time.Hour)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.sign(in.User, exp), Path: "/", Expires: exp,
		HttpOnly: true, Secure: *s.Cfg.Auth.SecureCookie, SameSite: http.SameSiteStrictMode})
	s.Log.Info("login", "user", in.User, "ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"user": in.User, "demo": s.Demo})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: *s.Cfg.Auth.SecureCookie, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": userOf(r), "demo": s.Demo})
}

// --- dashboard -----------------------------------------------------------------

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Mon.Overview(r.Context()))
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	d, ok := s.Mon.Detail(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, "Repository nicht gefunden")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.Cfg.Borg.Timeout.D()+30*time.Second)
	defer cancel()
	if err := s.Mon.RefreshNow(ctx, r.PathValue("id")); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	d, _ := s.Mon.Detail(r.PathValue("id"))
	writeJSON(w, http.StatusOK, d)
}

// check starts borg check by hand (read-only, never --repair).
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil || !in.Confirm {
		fail(w, http.StatusBadRequest, "Start muss bestätigt werden")
		return
	}
	c, err := s.Mon.RunCheck(r.PathValue("id"), "manuell")
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	s.Log.Info("borg check started", "user", userOf(r), "repo", c.RepoID)
	writeJSON(w, http.StatusAccepted, c)
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.Cfg.RepoByID(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, "Repository nicht gefunden")
		return
	}
	ref, _, _ := s.Cfg.RepoByID(r.PathValue("id"))
	if !ref.Job.RestoreEnabled(s.Cfg.Restore.Enabled) {
		fail(w, http.StatusForbidden, "Restore-Tests sind für diesen Job nicht freigegeben")
		return
	}
	archive, path := r.URL.Query().Get("archive"), r.URL.Query().Get("path")
	if archive == "" || strings.HasPrefix(archive, "-") || strings.ContainsAny(archive, "/\x00") {
		fail(w, http.StatusBadRequest, "Archiv angeben")
		return
	}
	items, cut, berr := s.Mon.Backend.Files(r.Context(), repo, archive, strings.TrimPrefix(path, "/"), 300)
	if berr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": berr.Finding.Summary, "hint": berr.Finding.Hint})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "truncated": cut})
}

func (s *Server) runLog(w http.ResponseWriter, r *http.Request) {
	log, cut, ok := s.Mon.RunLog(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, "Lauf nicht gefunden")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"log": log, "cut": cut})
}

// --- restore tests ---------------------------------------------------------------

func (s *Server) planRestore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RepoID  string   `json:"repo_id"`
		Archive string   `json:"archive"`
		Paths   []string `json:"paths"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "ungültige Anfrage")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*s.Cfg.Borg.Timeout.D())
	defer cancel()
	p, err := s.Restore.PlanTest(ctx, in.RepoID, in.Archive, in.Paths)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) startRestore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PlanID  string `json:"plan_id"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody)).Decode(&in); err != nil || !in.Confirm {
		fail(w, http.StatusBadRequest, "Start muss bestätigt werden")
		return
	}
	t, err := s.Restore.Start(in.PlanID, userOf(r))
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	s.Log.Info("restore test started", "user", userOf(r), "repo", t.RepoID, "archive", t.Archive, "paths", len(t.Paths))
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) restoreTest(w http.ResponseWriter, r *http.Request) {
	var found *store.RestoreTest
	s.Mon.Store.Read(func(st *store.State) {
		for _, t := range st.RestoreTests {
			if t.ID == r.PathValue("id") {
				c := *t
				found = &c
			}
		}
	})
	if found == nil {
		fail(w, http.StatusNotFound, "Test nicht gefunden")
		return
	}
	writeJSON(w, http.StatusOK, found)
}

// --- reports from borgmatic ----------------------------------------------------------

// ping accepts the Healthchecks protocol as borgmatic sends it:
// /ping/<token> (success), /start, /fail, /log, and /<exit code> (wrapper).
func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodHead && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	token := r.PathValue("token")
	kind, code := monitor.PingSuccess, (*int)(nil)
	switch k := r.PathValue("kind"); k {
	case "":
	case "start":
		kind = monitor.PingStart
	case "fail":
		kind = monitor.PingFail
	case "log":
		kind = monitor.PingLog
	default:
		n, err := strconv.Atoi(k)
		if err != nil || n < 0 || n > 255 {
			http.Error(w, "unknown ping", http.StatusNotFound)
			return
		}
		kind, code = monitor.PingExit, &n
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxPingBody))
	source := "healthchecks-ping"
	if strings.Contains(r.UserAgent(), "borg-backup-monitor-report") || code != nil {
		source = "wrapper"
	}
	if err := s.Mon.Ping(token, kind, code, string(body), source); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("OK"))
}

// HashPassword is used by "borg-monitor -hash-password".
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	return string(b), err
}

// NewToken creates a ping token.
func NewToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
