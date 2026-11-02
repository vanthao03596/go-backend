package link

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/thaopv/go-backend/internal/apikey"
	"github.com/thaopv/go-backend/internal/server"
	"github.com/thaopv/go-backend/internal/server/middleware"
)

// Middleware is a standard net/http middleware.
type Middleware = func(http.Handler) http.Handler

// Handler exposes the link service over HTTP: Huma operations under the
// management API and a plain redirect handler at the root.
type Handler struct {
	svc           *Service
	log           *slog.Logger
	redirectLimit Middleware
}

// HandlerOption customises a Handler.
type HandlerOption func(*Handler)

// WithRedirectRateLimit wraps GET and HEAD /{code} with mw. Rate limiting is
// applied here rather than globally so health checks are never throttled.
func WithRedirectRateLimit(mw Middleware) HandlerOption {
	return func(h *Handler) { h.redirectLimit = mw }
}

// NewHandler builds a Handler around svc.
func NewHandler(svc *Service, log *slog.Logger, opts ...HandlerOption) *Handler {
	h := &Handler{svc: svc, log: log}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// CreateRateLimit restricts mw to the create-link request so the rest of the
// management API keeps its own budget. Mount it with server.WithAPIMiddleware
// ahead of authentication: an unauthenticated burst must be rejected before it
// costs a key lookup.
func CreateRateLimit(mw Middleware) Middleware {
	return chimw.Maybe(mw, func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.TrimSuffix(r.URL.Path, "/") == server.APIBasePath+"/links"
	})
}

// Register mounts the redirect on root and the CRUD operations on api.
func (h *Handler) Register(root chi.Router, api huma.API) {
	// HEAD is served so link previewers and uptime checks get the 302
	// instead of a 405; it does not count as a click.
	redirect := root
	if h.redirectLimit != nil {
		redirect = root.With(h.redirectLimit)
	}
	redirect.Get("/{code}", h.redirect)
	redirect.Head("/{code}", h.redirect)

	huma.Register(api, huma.Operation{
		OperationID:   "create-link",
		Method:        http.MethodPost,
		Path:          "/links",
		Summary:       "Create a short link",
		Tags:          []string{"links"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusUnauthorized, http.StatusConflict, http.StatusUnprocessableEntity},
	}, h.create)

	huma.Register(api, huma.Operation{
		OperationID: "get-link",
		Method:      http.MethodGet,
		Path:        "/links/{code}",
		Summary:     "Get a link and its click count",
		Tags:        []string{"links"},
		Errors:      []int{http.StatusUnauthorized, http.StatusNotFound},
	}, h.get)

	huma.Register(api, huma.Operation{
		OperationID: "list-links",
		Method:      http.MethodGet,
		Path:        "/links",
		Summary:     "List the caller's links, newest first",
		Tags:        []string{"links"},
		Errors:      []int{http.StatusUnauthorized, http.StatusUnprocessableEntity},
	}, h.list)

	huma.Register(api, huma.Operation{
		OperationID:   "delete-link",
		Method:        http.MethodDelete,
		Path:          "/links/{code}",
		Summary:       "Delete a link. Its short URL stops redirecting.",
		Tags:          []string{"links"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusNotFound},
	}, h.del)
}

// --- wire types

// linkBody is the JSON representation of a link. nameHint tags give it the
// schema name "Link" in the OpenAPI document.
type linkBody struct {
	Code       string     `json:"code" doc:"Short code" example:"aZ3kQ9x"`
	ShortURL   string     `json:"short_url" format:"uri" doc:"Public redirect URL"`
	URL        string     `json:"url" format:"uri" doc:"Target URL"`
	ClickCount int64      `json:"click_count" doc:"Redirects served so far"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty" doc:"When the link stops redirecting"`
	CreatedAt  time.Time  `json:"created_at"`
}

type createInput struct {
	Body struct {
		URL       string     `json:"url" format:"uri" maxLength:"2048" doc:"Absolute http or https URL to redirect to" example:"https://example.com/some/long/path"`
		Alias     string     `json:"alias,omitempty" minLength:"3" maxLength:"32" pattern:"^[A-Za-z0-9_-]+$" doc:"Optional custom code"`
		ExpiresAt *time.Time `json:"expires_at,omitempty" doc:"Optional expiry, RFC 3339"`
	}
}

type createOutput struct {
	Location string   `header:"Location"`
	Body     linkBody `nameHint:"Link"`
}

type codeInput struct {
	Code string `path:"code" minLength:"3" maxLength:"32" pattern:"^[A-Za-z0-9_-]+$"`
}

type linkOutput struct {
	Body linkBody `nameHint:"Link"`
}

type listInput struct {
	Cursor string `query:"cursor" doc:"Opaque cursor from a previous page"`
	Limit  int32  `query:"limit" minimum:"1" maximum:"100" doc:"Page size, default 20"`
}

type listOutput struct {
	Body struct {
		Items      []linkBody `json:"items" nameHint:"Link"`
		NextCursor string     `json:"next_cursor,omitempty" doc:"Pass as cursor to fetch the next page; absent on the last page"`
	}
}

// --- handlers

func (h *Handler) create(ctx context.Context, in *createInput) (*createOutput, error) {
	keyID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	l, err := h.svc.Create(ctx, keyID, CreateParams{
		URL:       in.Body.URL,
		Alias:     in.Body.Alias,
		ExpiresAt: in.Body.ExpiresAt,
	})
	if err != nil {
		return nil, h.toHTTP(ctx, err)
	}
	return &createOutput{Location: h.svc.ShortURL(l.Code), Body: h.body(l)}, nil
}

func (h *Handler) get(ctx context.Context, in *codeInput) (*linkOutput, error) {
	keyID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	l, err := h.svc.Get(ctx, keyID, in.Code)
	if err != nil {
		return nil, h.toHTTP(ctx, err)
	}
	return &linkOutput{Body: h.body(l)}, nil
}

func (h *Handler) list(ctx context.Context, in *listInput) (*listOutput, error) {
	keyID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	links, next, err := h.svc.List(ctx, keyID, in.Cursor, in.Limit)
	if err != nil {
		return nil, h.toHTTP(ctx, err)
	}
	out := &listOutput{}
	out.Body.Items = make([]linkBody, 0, len(links))
	for _, l := range links {
		out.Body.Items = append(out.Body.Items, h.body(l))
	}
	out.Body.NextCursor = next
	return out, nil
}

func (h *Handler) del(ctx context.Context, in *codeInput) (*struct{}, error) {
	keyID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.Delete(ctx, keyID, in.Code); err != nil {
		return nil, h.toHTTP(ctx, err)
	}
	return nil, nil //nolint:nilnil // Huma: nil output with DefaultStatus 204
}

// redirect is a plain handler on the hot path. It deliberately avoids Huma.
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

	target, err := h.svc.Resolve(r.Context(), code, r.Method != http.MethodHead)
	switch {
	case err == nil:
		// Redirecting to a caller-supplied URL is the product. The target was
		// validated by ValidateTargetURL when the link was created.
		http.Redirect(w, r, target, http.StatusFound) //nolint:gosec // G710 open redirect is intentional
	case errors.Is(err, ErrNotFound):
		http.Error(w, "short link not found", http.StatusNotFound)
	case errors.Is(err, ErrGone):
		http.Error(w, "short link expired or deleted", http.StatusGone)
	default:
		h.log.ErrorContext(r.Context(), "resolve link",
			"request_id", middleware.RequestIDFrom(r.Context()), "code", code, "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

// --- helpers

func callerID(ctx context.Context) (int64, error) {
	id, ok := apikey.IDFromContext(ctx)
	if !ok {
		return 0, huma.Error401Unauthorized("missing or invalid API key")
	}
	return id, nil
}

func (h *Handler) body(l Link) linkBody {
	b := linkBody{
		Code:       l.Code,
		ShortURL:   h.svc.ShortURL(l.Code),
		URL:        l.TargetURL,
		ClickCount: l.ClickCount,
		CreatedAt:  l.CreatedAt.UTC(),
	}
	if l.ExpiresAt != nil {
		exp := l.ExpiresAt.UTC()
		b.ExpiresAt = &exp
	}
	return b
}

// toHTTP maps domain errors to problem-details responses. Anything unmapped
// is logged with the request id and returned as a generic 500 so internal
// detail (SQL state, driver messages) never reaches the client.
func (h *Handler) toHTTP(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return huma.Error404NotFound("link not found")
	case errors.Is(err, ErrCodeTaken):
		return huma.Error409Conflict("alias already in use")
	case errors.Is(err, ErrInvalidURL),
		errors.Is(err, ErrInvalidAlias),
		errors.Is(err, ErrReservedAlias),
		errors.Is(err, ErrInvalidExpiry),
		errors.Is(err, ErrInvalidCursor):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		h.log.ErrorContext(ctx, "link handler",
			"request_id", middleware.RequestIDFrom(ctx), "err", err)
		return huma.Error500InternalServerError("internal error")
	}
}
