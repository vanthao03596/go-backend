package server

import (
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/thaopv/go-backend/internal/server/middleware"
)

// APIBasePath is where the Huma-managed management API is mounted.
const APIBasePath = "/api/v1"

// SecuritySchemeName is the OpenAPI security scheme every /api/v1 operation
// requires. The matching middleware is installed with WithAPIMiddleware.
const SecuritySchemeName = "bearerAuth"

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.ClientIP(s.cfg.TrustedProxies))
	r.Use(middleware.Recover(s.log))
	r.Use(middleware.Logger(s.log))
	r.Use(middleware.Timeout(s.cfg.RequestTimeout))

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.Route(APIBasePath, func(api chi.Router) {
		cfg := huma.DefaultConfig("URL Shortener API", "1.0.0")
		cfg.Info.Description = "Create short links and track redirects."

		// Docs, the spec and schemas are public; everything else on the group
		// goes through the API middleware (rate limiting, authentication).
		protected := func(r *http.Request) bool { return !isPublicAPIPath(cfg, r.URL.Path) }
		for _, mw := range s.apiMiddleware {
			api.Use(chimw.Maybe(mw, protected))
		}

		cfg.Servers = []*huma.Server{{URL: s.cfg.BaseURL + APIBasePath}}
		if cfg.Components == nil {
			cfg.Components = &huma.Components{}
		}
		cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
			SecuritySchemeName: {
				Type:        "http",
				Scheme:      "bearer",
				Description: "API key issued with `task apikey:create -- <name>`.",
			},
		}
		cfg.Security = []map[string][]string{{SecuritySchemeName: {}}}
		hapi := humachi.New(api, cfg)

		for _, f := range s.features {
			f(r, hapi)
		}
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
	})

	return r
}

// isPublicAPIPath reports whether path under APIBasePath is one of Huma's
// documentation routes, which need no credentials.
func isPublicAPIPath(cfg huma.Config, path string) bool {
	p := strings.TrimPrefix(path, APIBasePath)
	switch {
	case cfg.DocsPath != "" && p == cfg.DocsPath:
		return true
	case cfg.OpenAPIPath != "" && (strings.HasPrefix(p, cfg.OpenAPIPath+".") || strings.HasPrefix(p, cfg.OpenAPIPath+"-")):
		// Huma serves openapi.{json,yaml} and the 3.0 downgrades openapi-3.0.{json,yaml}.
		return true
	case cfg.SchemasPath != "" && strings.HasPrefix(p, cfg.SchemasPath+"/"):
		return true
	}
	return false
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
