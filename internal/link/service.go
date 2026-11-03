package link

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	// maxCodeAttempts bounds retries when a generated code collides.
	maxCodeAttempts = 3
	// DefaultPageSize is used when the caller omits a limit.
	DefaultPageSize int32 = 20
	// MaxPageSize caps the caller-supplied limit.
	MaxPageSize int32 = 100
)

// instrumentationName is the OpenTelemetry scope for link metrics.
const instrumentationName = "github.com/vanthao03596/go-backend/internal/link"

// Metric attribute for link creations: whether the caller chose the code.
const attrCustomAlias = attribute.Key("shortener.link.custom_alias")

var (
	createdWithAlias = metric.WithAttributeSet(attribute.NewSet(attrCustomAlias.Bool(true)))
	createdGenerated = metric.WithAttributeSet(attribute.NewSet(attrCustomAlias.Bool(false)))
)

// Service holds the business rules. It knows nothing about HTTP or SQL.
type Service struct {
	repo    Repository
	clicks  ClickSink
	baseURL string
	log     *slog.Logger
	now     func() time.Time

	meterProvider metric.MeterProvider
	meter         metric.Meter
	created       metric.Int64Counter
	collisions    metric.Int64Counter
}

// ServiceOption customises a Service at construction time.
type ServiceOption func(*Service)

// WithNow replaces the clock the service uses for expiry decisions. Tests
// inject a controllable clock so expiry can be exercised without sleeping.
func WithNow(now func() time.Time) ServiceOption {
	return func(s *Service) { s.now = now }
}

// WithClickSink sets where counted redirects are sent. Without it clicks are
// discarded; the composition root always installs the click buffer.
func WithClickSink(sink ClickSink) ServiceOption {
	return func(s *Service) { s.clicks = sink }
}

// WithMeterProvider sets where the service and its Handler report metrics.
// Without it the otel global is used, a no-op until telemetry is set up.
func WithMeterProvider(mp metric.MeterProvider) ServiceOption {
	return func(s *Service) { s.meterProvider = mp }
}

// NewService wires a Service. baseURL is the public origin used to build short URLs.
func NewService(repo Repository, baseURL string, log *slog.Logger, opts ...ServiceOption) *Service {
	s := &Service{
		repo:    repo,
		clicks:  discardClicks{},
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.meterProvider == nil {
		s.meterProvider = otel.GetMeterProvider()
	}
	s.meter = s.meterProvider.Meter(instrumentationName)

	var errs [2]error
	s.created, errs[0] = s.meter.Int64Counter("shortener.links.created",
		metric.WithDescription("Links created, by whether the caller chose the code."),
		metric.WithUnit("{link}"))
	s.collisions, errs[1] = s.meter.Int64Counter("shortener.links.code_collisions",
		metric.WithDescription("Generated codes that collided with an existing code and were regenerated."),
		metric.WithUnit("{collision}"))
	// The SDK returns a working instrument alongside a registration error,
	// so report it and carry on rather than fail construction.
	if err := errors.Join(errs[:]...); err != nil {
		otel.Handle(err)
	}
	return s
}

// ShortURL renders the public URL for a code.
func (s *Service) ShortURL(code string) string {
	return s.baseURL + "/" + code
}

// Create validates params and stores a new link owned by apiKeyID.
func (s *Service) Create(ctx context.Context, apiKeyID int64, p CreateParams) (Link, error) {
	if err := ValidateTargetURL(p.URL); err != nil {
		return Link{}, err
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(s.now()) {
		return Link{}, ErrInvalidExpiry
	}

	if p.Alias != "" {
		if err := ValidateAlias(p.Alias); err != nil {
			return Link{}, err
		}
		l, err := s.repo.Insert(ctx, p.Alias, p.URL, apiKeyID, p.ExpiresAt)
		if err != nil {
			return Link{}, err
		}
		s.created.Add(ctx, 1, createdWithAlias)
		return l, nil
	}

	for attempt := 1; attempt <= maxCodeAttempts; attempt++ {
		code, err := GenerateCode()
		if err != nil {
			return Link{}, err
		}
		l, err := s.repo.Insert(ctx, code, p.URL, apiKeyID, p.ExpiresAt)
		if err == nil {
			s.created.Add(ctx, 1, createdGenerated)
			return l, nil
		}
		if !errors.Is(err, ErrCodeTaken) {
			return Link{}, err
		}
		s.collisions.Add(ctx, 1)
		s.log.WarnContext(ctx, "generated code collided", "attempt", attempt, "code", code)
	}
	return Link{}, fmt.Errorf("gave up after %d code collisions", maxCodeAttempts)
}

// Get returns a live link owned by apiKeyID.
func (s *Service) Get(ctx context.Context, apiKeyID int64, code string) (Link, error) {
	return s.repo.GetByCodeForKey(ctx, code, apiKeyID)
}

// Delete soft-deletes a live link owned by apiKeyID.
func (s *Service) Delete(ctx context.Context, apiKeyID int64, code string) error {
	return s.repo.SoftDelete(ctx, code, apiKeyID)
}

// List returns one page of the caller's links and the cursor for the next
// page, or an empty cursor when there are no more rows.
func (s *Service) List(ctx context.Context, apiKeyID int64, encodedCursor string, limit int32) ([]Link, string, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	limit = min(limit, MaxPageSize)

	cursor, err := decodeCursor(encodedCursor)
	if err != nil {
		return nil, "", err
	}

	// Fetch one extra row to learn whether another page exists.
	rows, err := s.repo.List(ctx, apiKeyID, cursor, limit+1)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > int(limit) {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = encodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return rows, next, nil
}

// Resolve returns the target URL for a redirect. When visit is non-nil the
// click is handed to the ClickSink; callers pass nil for HEAD requests so
// previewers and health checks do not inflate the count. Recording is
// non-blocking and cannot fail the redirect.
// It returns ErrNotFound for unknown codes and ErrGone for deleted or expired links.
func (s *Service) Resolve(ctx context.Context, code string, visit *Visit) (string, error) {
	l, err := s.repo.GetByCode(ctx, code)
	if err != nil {
		return "", err
	}
	now := s.now()
	if l.DeletedAt != nil || l.Expired(now) {
		return "", ErrGone
	}
	if visit != nil {
		s.clicks.Record(Click{
			LinkID:     l.ID,
			OccurredAt: now,
			Referrer:   visit.Referrer,
			UserAgent:  visit.UserAgent,
		})
	}
	return l.TargetURL, nil
}

// discardClicks is the default sink: it drops every click.
type discardClicks struct{}

func (discardClicks) Record(Click) {}

// Cursor encoding: base64url("<unix nanos>:<id>"). Opaque to clients.

func encodeCursor(c Cursor) string {
	raw := strconv.FormatInt(c.CreatedAt.UnixNano(), 10) + ":" + strconv.FormatInt(c.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{CreatedAt: time.Unix(0, math.MaxInt64), ID: math.MaxInt64}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	ts, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return Cursor{}, ErrInvalidCursor
	}
	nanos, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	rowID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{CreatedAt: time.Unix(0, nanos), ID: rowID}, nil
}
