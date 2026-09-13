// Package server exposes the manager over HTTP: a JSON REST API plus the
// embedded single-page management UI.
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"

	"github.com/fatedier/frp/pkg/util/version"

	"frp-more/internal/logbuf"
	"frp-more/internal/manager"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	mgr  *manager.Manager
	logs *logbuf.Buffer
}

func New(mgr *manager.Manager, logs *logbuf.Buffer) *http.Server {
	s := &Server{mgr: mgr, logs: logs}
	mux := http.NewServeMux()

	// API
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /api/instances", s.handleList)
	mux.HandleFunc("POST /api/instances", s.handleCreate)
	mux.HandleFunc("POST /api/reload-dir", s.handleReloadDir)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
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
	return &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})}
}

type versionInfo struct {
	AppVersion string `json:"appVersion"`
	FrpVersion string `json:"frpVersion"`
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, versionInfo{
		AppVersion: "0.1.9",
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

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Settings())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var p manager.Settings
	if err := readJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.mgr.SetSettings(p); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
