package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/arara-ide/arara-agent-gateway/internal/connect"
	"github.com/arara-ide/arara-agent-gateway/internal/docker"
	"github.com/arara-ide/arara-agent-gateway/internal/github"
	"github.com/arara-ide/arara-agent-gateway/internal/instance"
	"github.com/arara-ide/arara-agent-gateway/internal/run"
	"github.com/arara-ide/arara-agent-gateway/internal/store"
)

const maxBodyBytes = 1 << 20

type Deps struct {
	Store       *store.Store
	Engine      docker.Engine
	Instances   *instance.Manager
	Runs        *run.Manager
	Connections *connect.Service
	GitHub      *github.Service
	Workspace   *github.Workspace
	APIKey      string
	IdleTimeout time.Duration
	AutoStart   bool
	Web         fs.FS
	OpenAPI     []byte
	Log         *slog.Logger
	Version     string
}

type Server struct {
	Deps
	mux *http.ServeMux
}

func New(d Deps) *Server {
	s := &Server{Deps: d, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.mux.ServeHTTP(rec, r)
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.Log.Debug("request", "method", r.Method, "path", r.Pattern, "status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

type handler func(w http.ResponseWriter, r *http.Request) error

func (s *Server) handle(pattern string, h handler) {
	s.mux.Handle(pattern, s.auth(h))
}

func (s *Server) auth(h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		key := r.Header.Get("apikey")
		if key == "" || !equal(key, s.APIKey) {
			writeError(w, errUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := h(w, r); err != nil {
			ae := toAPIError(err)
			if ae.status >= 500 {
				s.Log.Error("request failed", "path", r.Pattern, "error", err)
			}
			writeError(w, ae)
		}
	})
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) instanceParam(r *http.Request) (store.Instance, error) {
	return s.Store.GetInstance(r.Context(), r.PathValue("id"))
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errBadJSON
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return newError(http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
		}
		return errBadJSON
	}
	if dec.More() {
		return errBadJSON
	}
	return nil
}

func decodeOptional(r *http.Request, v any) error {
	err := decode(r, v)
	if err == errBadJSON && r.ContentLength == 0 {
		return nil
	}
	return err
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(s.OpenAPI)
	})

	s.handle("GET /api/agents", s.listAgents)

	s.handle("GET /api/instances", s.listInstances)
	s.handle("POST /api/instances", s.createInstance)
	s.handle("GET /api/instances/metrics", s.listMetrics)
	s.handle("GET /api/instances/{id}", s.getInstance)
	s.handle("PATCH /api/instances/{id}", s.updateInstance)
	s.handle("DELETE /api/instances/{id}", s.deleteInstance)
	s.handle("POST /api/instances/{id}/start", s.startInstance)
	s.handle("POST /api/instances/{id}/stop", s.stopInstance)
	s.handle("GET /api/instances/{id}/status", s.instanceStatus)
	s.handle("GET /api/instances/{id}/history", s.history)
	s.handle("GET /api/instances/{id}/connections", s.instanceConnections)

	s.handle("GET /api/instances/{id}/agents", s.instanceAgents)
	s.handle("GET /api/instances/{id}/agents/{agent}/models", s.listAgentModels)
	s.handle("GET /api/instances/{id}/agents/{agent}/usage", s.agentUsage)
	s.handle("PUT /api/instances/{id}/agents/{agent}/api-key", s.setAPIKey)
	s.handle("DELETE /api/instances/{id}/agents/{agent}/api-key", s.removeAPIKey)
	s.handle("POST /api/instances/{id}/agents/{agent}/run", s.startRun)
	s.handle("POST /api/instances/{id}/agents/{agent}/login", s.startLogin)
	s.handle("GET /api/instances/{id}/agents/{agent}/login", s.getLogin)
	s.handle("POST /api/instances/{id}/agents/{agent}/login/input", s.loginInput)
	s.handle("DELETE /api/instances/{id}/agents/{agent}/login", s.cancelLogin)
	s.handle("POST /api/instances/{id}/agents/{agent}/logout", s.logout)

	s.handle("GET /api/instances/{id}/runs/{runId}", s.getRun)
	s.handle("GET /api/instances/{id}/runs/{runId}/stream", s.streamRun)
	s.handle("POST /api/instances/{id}/runs/{runId}/cancel", s.cancelRun)

	s.handle("POST /api/instances/{id}/github/connect", s.githubConnect)
	s.handle("POST /api/instances/{id}/github/callback", s.githubCallback)
	s.handle("GET /api/instances/{id}/github", s.githubStatus)
	s.handle("GET /api/instances/{id}/github/repos", s.githubRepos)

	s.handle("GET /api/instances/{id}/repos", s.listLocalRepos)
	s.handle("GET /api/instances/{id}/repos/changes", s.repoChanges)
	s.handle("POST /api/instances/{id}/repos", s.syncRepo)
	s.handle("POST /api/instances/{id}/repos/{owner}/{name}/push", s.pushRepo)

	s.mux.Handle("/", s.fallback())
}

func (s *Server) fallback() http.Handler {
	var files http.Handler
	if s.Web != nil {
		files = s.dashboard()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || files == nil {
			writeError(w, errNotFound)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, newError(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed"))
			return
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) dashboard() http.Handler {
	files := http.FileServerFS(s.Web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		files.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	status, code := "ok", http.StatusOK
	checks := map[string]string{"database": "ok", "docker": "ok"}
	if err := s.Store.Ping(ctx); err != nil {
		checks["database"], status, code = "error", "degraded", http.StatusServiceUnavailable
	}
	if err := s.Engine.Ping(ctx); err != nil {
		checks["docker"], status, code = "error", "degraded", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": checks, "version": s.Version})
}
