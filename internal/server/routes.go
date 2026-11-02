package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/thaopv/go-backend/internal/server/middleware"
)

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Recover(s.log))
	r.Use(middleware.Logger(s.log))
	r.Use(middleware.Timeout(s.cfg.RequestTimeout))

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
	})

	return r
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	for name, check := range s.readiness {
		if err := check(r.Context()); err != nil {
			s.log.WarnContext(r.Context(), "readiness check failed", "check", name, "err", err)
			writeError(w, http.StatusServiceUnavailable, "not_ready", name+" unavailable")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
