package click

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/vanthao03596/go-backend/internal/apikey"
	"github.com/vanthao03596/go-backend/internal/link"
	"github.com/vanthao03596/go-backend/internal/server/middleware"
)

// Handler exposes link statistics on the management API. Ownership is
// checked through link.Service so a caller only ever sees its own links.
type Handler struct {
	svc   *Service
	links *link.Service
	log   *slog.Logger
}

// NewHandler builds a Handler.
func NewHandler(svc *Service, links *link.Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, links: links, log: log}
}

// Register mounts GET /links/{code}/stats on api.
func (h *Handler) Register(_ chi.Router, api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-link-stats",
		Method:      http.MethodGet,
		Path:        "/links/{code}/stats",
		Summary:     "Click statistics for a link",
		Description: "Lifetime total plus clicks per UTC day for the last 7 days. Counts are flushed in batches, so the latest second may be missing.",
		Tags:        []string{"links"},
		Errors:      []int{http.StatusUnauthorized, http.StatusNotFound},
	}, h.stats)
}

type statsInput struct {
	Code string `path:"code" minLength:"3" maxLength:"32" pattern:"^[A-Za-z0-9_-]+$"`
}

type dayBody struct {
	Date   string `json:"date" format:"date" doc:"UTC calendar day" example:"2026-11-02"`
	Clicks int64  `json:"clicks" doc:"Redirects served that day"`
}

// linkStats is the JSON body of the stats operation. Huma names the schema
// after the Go type, so it appears as LinkStats in the OpenAPI document.
type linkStats struct {
	Code        string    `json:"code" doc:"Short code" example:"aZ3kQ9x"`
	TotalClicks int64     `json:"total_clicks" doc:"Lifetime redirects, including days outside the window"`
	Days        []dayBody `json:"days" doc:"Last 7 UTC days, oldest first, zero-filled"`
}

type statsOutput struct {
	Body linkStats
}

func (h *Handler) stats(ctx context.Context, in *statsInput) (*statsOutput, error) {
	keyID, ok := apikey.IDFromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("missing or invalid API key")
	}
	l, err := h.links.Get(ctx, keyID, in.Code)
	if err != nil {
		return nil, h.toHTTP(ctx, err)
	}
	st, err := h.svc.Stats(ctx, l.ID, l.ClickCount)
	if err != nil {
		return nil, h.toHTTP(ctx, err)
	}

	out := &statsOutput{Body: linkStats{Code: l.Code, TotalClicks: st.Total}}
	out.Body.Days = make([]dayBody, 0, len(st.Days))
	for _, d := range st.Days {
		out.Body.Days = append(out.Body.Days, dayBody{Date: d.Day.Format(time.DateOnly), Clicks: d.Clicks})
	}
	return out, nil
}

func (h *Handler) toHTTP(ctx context.Context, err error) error {
	if errors.Is(err, link.ErrNotFound) {
		return huma.Error404NotFound("link not found")
	}
	h.log.ErrorContext(ctx, "click stats handler",
		"request_id", middleware.RequestIDFrom(ctx), "err", err)
	return huma.Error500InternalServerError("internal error")
}
