// Package server exposes the manager over HTTP: a JSON REST API plus the
// embedded single-page management UI.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fatedier/frp/pkg/util/version"

	"frp-more/internal/logbuf"
	"frp-more/internal/manager"
)

//go:embed static
var staticFS embed.FS

type AuthConfig struct {
	Username string
	Password string
}

type Server struct {
	mgr  *manager.Manager
	logs *logbuf.Buffer
	auth AuthConfig

	sessionsMu sync.Mutex
	sessions   map[string]time.Time
}

const (
	sessionCookie = "frp_more_session"
	sessionTTL    = 24 * time.Hour
)

func New(mgr *manager.Manager, logs *logbuf.Buffer, auth AuthConfig) *http.Server {
	if auth.Username == "" {
		auth.Username = "admin"
	}
	if auth.Password == "" {
		auth.Password = "admin123"
	}
	s := &Server{
		mgr:      mgr,
		logs:     logs,
		auth:     auth,
		sessions: make(map[string]time.Time),
	}
	mux := http.NewServeMux()

	// Authentication API. The version and session endpoints are public so the
	// embedded UI can decide whether to show the login form.
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/logout", s.handleLogout)

	// API
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /api/instances", s.handleList)
	mux.HandleFunc("POST /api/instances", s.handleCreate)
	mux.HandleFunc("POST /api/reload-dir", s.handleReloadDir)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/instances/{name}/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/instances/{name}/config", s.handlePutConfig)
	mux.HandleFunc("POST /api/instances/{name}/start", s.handleStart)
	mux.HandleFunc("POST /api/instances/{name}/stop", s.handleStop)
	mux.HandleFunc("POST /api/instances/{name}/restart", s.handleRestart)
	mux.HandleFunc("DELETE /api/instances/{name}", s.handleDelete)

	// UI
	staticRoot, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticRoot))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, staticFS, "static/index.html")
	})

	// noStore stops browsers from caching responses, so UI and API updates
	// are always picked up without a manual hard refresh.
	return &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if strings.HasPrefix(r.URL.Path, "/api/") && !s.publicAPI(r) && !s.authenticated(r) {
				writeErr(w, http.StatusUnauthorized, errors.New("authentication required"))
				return
			}
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func (s *Server) publicAPI(r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/login":
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/api/session":
		return true
	case r.Method == http.MethodPost && r.URL.Path == "/api/logout":
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/api/version":
		return true
	default:
		return false
	}
}

func (s *Server) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := time.Now()
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	expires, ok := s.sessions[cookie.Value]
	if !ok {
		return false
	}
	if now.After(expires) {
		delete(s.sessions, cookie.Value)
		return false
	}
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(p.Username), []byte(s.auth.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(p.Password), []byte(s.auth.Password)) == 1
	if !userOK || !passOK {
		writeErr(w, http.StatusUnauthorized, errors.New("invalid username or password"))
		return
	}

	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		writeErr(w, http.StatusInternalServerError, errors.New("create session failed"))
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	s.sessionsMu.Lock()
	s.sessions[token] = time.Now().Add(sessionTTL)
	s.sessionsMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "username": s.auth.Username})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": s.auth.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.sessionsMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type versionInfo struct {
	AppVersion string `json:"appVersion"`
	FrpVersion string `json:"frpVersion"`
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, versionInfo{
		AppVersion: "0.1.10",
		FrpVersion: version.Full(),
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.List())
}

func (s *Server) handleReloadDir(w http.ResponseWriter, r *http.Request) {
	s.mgr.ScanDir()
	writeJSON(w, http.StatusOK, s.mgr.List())
}

type logsPayload struct {
	Lines   []string `json:"lines"`
	Dropped int      `json:"dropped"`
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	lines, dropped := s.logs.Snapshot()
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, logsPayload{Lines: lines, Dropped: dropped})
}

type instancePayload struct {
	Name   string `json:"name"`
	Config string `json:"config"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var p instancePayload
	if err := readJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if p.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	if err := s.mgr.Create(p.Name, p.Config); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	content, err := s.mgr.GetConfig(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, instancePayload{Config: content})
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var p instancePayload
	if err := readJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.mgr.UpdateConfig(r.PathValue("name"), p.Name, p.Config); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Start(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Stop(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Restart(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}
