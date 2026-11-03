package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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

// Metric attributes for the redirect cache.
const (
	attrCacheResult    = attribute.Key("shortener.cache.result")
	attrCacheOperation = attribute.Key("shortener.cache.operation")
)

var (
	createdWithAlias = metric.WithAttributeSet(attribute.NewSet(attrCustomAlias.Bool(true)))
	createdGenerated = metric.WithAttributeSet(attribute.NewSet(attrCustomAlias.Bool(false)))

	cacheHit       = metric.WithAttributeSet(attribute.NewSet(attrCacheResult.String("hit")))
	cacheMiss      = metric.WithAttributeSet(attribute.NewSet(attrCacheResult.String("miss")))
	cacheError     = metric.WithAttributeSet(attribute.NewSet(attrCacheResult.String("error")))
	cacheFillError = metric.WithAttributeSet(attribute.NewSet(attrCacheOperation.String("fill")))
	cachePutError  = metric.WithAttributeSet(attribute.NewSet(attrCacheOperation.String("put")))
)

const (
	// cacheRepairInterval is how often failed tombstone writes are retried.
	cacheRepairInterval = time.Second
	// maxPendingTombstones bounds the repair queue. Deletes are rare, so
	// overflowing it takes a long Redis outage; the overflow is logged.
	maxPendingTombstones = 10_000
)

// Service holds the business rules. It knows nothing about HTTP or SQL.
type Service struct {
	repo    Repository
	clicks  ClickSink
	cache   Cache // nil when redirects always read the database
	cursors cursorSigner
	baseURL string
	log     *slog.Logger
	now     func() time.Time

	cursorSecret []byte

	// pendingTombstones holds codes whose deletion has not reached the
	// cache yet. RunCacheRepair retries them.
	mu                sync.Mutex
	pendingTombstones map[string]struct{}

	meterProvider    metric.MeterProvider
	meter            metric.Meter
	created          metric.Int64Counter
	collisions       metric.Int64Counter
	cacheLookups     metric.Int64Counter
	cacheWriteErrors metric.Int64Counter
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

// WithCache puts c in front of the redirect lookup. Without it every
// redirect reads the database.
func WithCache(c Cache) ServiceOption {
	return func(s *Service) { s.cache = c }
}

// WithCursorSecret sets the key that signs list cursors. Every replica must
// share it. Without it a random key is used, so cursors do not survive a
// restart.
func WithCursorSecret(secret []byte) ServiceOption {
	return func(s *Service) { s.cursorSecret = secret }
}

// WithMeterProvider sets where the service and its Handler report metrics.
// Without it the otel global is used, a no-op until telemetry is set up.
func WithMeterProvider(mp metric.MeterProvider) ServiceOption {
	return func(s *Service) { s.meterProvider = mp }
}

// NewService wires a Service. baseURL is the public origin used to build short URLs.
func NewService(repo Repository, baseURL string, log *slog.Logger, opts ...ServiceOption) *Service {
	s := &Service{
		repo:              repo,
		clicks:            discardClicks{},
		baseURL:           strings.TrimRight(baseURL, "/"),
		log:               log,
		now:               time.Now,
		pendingTombstones: map[string]struct{}{},
	}
	for _, opt := range opts {
		opt(s)
	}
	s.cursors = newCursorSigner(s.cursorSecret)
	if s.meterProvider == nil {
		s.meterProvider = otel.GetMeterProvider()
	}
	s.meter = s.meterProvider.Meter(instrumentationName)

	var errs [4]error
	s.created, errs[0] = s.meter.Int64Counter("shortener.links.created",
		metric.WithDescription("Links created, by whether the caller chose the code."),
		metric.WithUnit("{link}"))
	s.collisions, errs[1] = s.meter.Int64Counter("shortener.links.code_collisions",
		metric.WithDescription("Generated codes that collided with an existing code and were regenerated."),
		metric.WithUnit("{collision}"))
	s.cacheLookups, errs[2] = s.meter.Int64Counter("shortener.links.cache.lookups",
		metric.WithDescription("Redirect lookups against the cache, by result: hit, miss, or error (served from the database)."),
		metric.WithUnit("{lookup}"))
	s.cacheWriteErrors, errs[3] = s.meter.Int64Counter("shortener.links.cache.write_errors",
		metric.WithDescription("Failed cache writes, by operation: fill after a database read, or put after a create or delete."),
		metric.WithUnit("{error}"))
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
		s.cacheCreated(ctx, l)
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
			s.cacheCreated(ctx, l)
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

// Delete soft-deletes a live link owned by apiKeyID. Its redirect answers
// 410 from then on, including from the cache.
func (s *Service) Delete(ctx context.Context, apiKeyID int64, code string) error {
	if err := s.repo.SoftDelete(ctx, code, apiKeyID); err != nil {
		return err
	}
	s.cacheDeleted(ctx, code)
	return nil
}

// List returns one page of the caller's links and the cursor for the next
// page, or an empty cursor when there are no more rows.
func (s *Service) List(ctx context.Context, apiKeyID int64, encodedCursor string, limit int32) ([]Link, string, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	limit = min(limit, MaxPageSize)

	cursor, err := s.cursors.decode(apiKeyID, encodedCursor)
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
		next = s.cursors.encode(apiKeyID, Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return rows, next, nil
}

// Resolve returns the target URL for a redirect. When visit is non-nil the
// click is handed to the ClickSink; callers pass nil for HEAD requests so
// previewers and health checks do not inflate the count. Recording is
// non-blocking and cannot fail the redirect.
// It returns ErrNotFound for unknown codes and ErrGone for deleted or expired links.
func (s *Service) Resolve(ctx context.Context, code string, visit *Visit) (string, error) {
	d, err := s.destination(ctx, code)
	if err != nil {
		return "", err
	}
	now := s.now()
	switch {
	case d.Missing:
		return "", ErrNotFound
	case d.Deleted || d.Expired(now):
		return "", ErrGone
	}
	if visit != nil {
		s.clicks.Record(Click{
			LinkID:     d.LinkID,
			OccurredAt: now,
			Referrer:   visit.Referrer,
			UserAgent:  visit.UserAgent,
		})
	}
	return d.URL, nil
}

// destination looks code up in the cache, falling back to the database on a
// miss or a cache error. A miss is filled from the database result; an error
// is not, since a failing cache would only fail again.
func (s *Service) destination(ctx context.Context, code string) (Destination, error) {
	if s.cache == nil {
		return s.loadDestination(ctx, code)
	}
	d, ok, err := s.cache.Get(ctx, code)
	switch {
	case err != nil:
		s.cacheLookups.Add(ctx, 1, cacheError)
		s.log.DebugContext(ctx, "link cache get failed", "code", code, "err", err)
		return s.loadDestination(ctx, code)
	case ok:
		s.cacheLookups.Add(ctx, 1, cacheHit)
		return d, nil
	}
	s.cacheLookups.Add(ctx, 1, cacheMiss)
	d, err = s.loadDestination(ctx, code)
	if err != nil {
		return Destination{}, err
	}
	if err := s.cache.Fill(ctx, code, d); err != nil {
		s.cacheWriteErrors.Add(ctx, 1, cacheFillError)
		s.log.DebugContext(ctx, "link cache fill failed", "code", code, "err", err)
	}
	return d, nil
}

func (s *Service) loadDestination(ctx context.Context, code string) (Destination, error) {
	l, err := s.repo.GetByCode(ctx, code)
	if errors.Is(err, ErrNotFound) {
		return Destination{Missing: true}, nil
	}
	if err != nil {
		return Destination{}, err
	}
	return destinationOf(l), nil
}

// cacheCreated replaces any negative entry for a new link's code. If the
// write fails, a code probed just before its creation can answer 404 for
// up to the negative TTL; that is logged and not retried.
func (s *Service) cacheCreated(ctx context.Context, l Link) {
	if s.cache == nil {
		return
	}
	// Detached from cancellation: the link exists whether or not the client
	// waits for the response.
	if err := s.cache.Put(context.WithoutCancel(ctx), l.Code, destinationOf(l)); err != nil {
		s.cacheWriteErrors.Add(ctx, 1, cachePutError)
		s.log.WarnContext(ctx, "link cache put after create failed; a cached 404 may linger for the negative TTL",
			"code", l.Code, "err", err)
	}
}

// cacheDeleted writes the tombstone for a deleted code. If the write fails,
// a cached destination could keep redirecting until its TTL runs out, so the
// code is queued for RunCacheRepair, which retries until Redis takes it.
func (s *Service) cacheDeleted(ctx context.Context, code string) {
	if s.cache == nil {
		return
	}
	err := s.cache.Put(context.WithoutCancel(ctx), code, Destination{Deleted: true})
	if err == nil {
		return
	}
	s.cacheWriteErrors.Add(ctx, 1, cachePutError)
	if s.queueTombstone(code) {
		s.log.WarnContext(ctx, "link cache tombstone write failed; retrying in the background", "code", code, "err", err)
	} else {
		s.log.ErrorContext(ctx, "link cache tombstone write failed and the repair queue is full; the link may redirect until its cache TTL expires",
			"code", code, "err", err)
	}
}

func (s *Service) queueTombstone(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingTombstones) >= maxPendingTombstones {
		return false
	}
	s.pendingTombstones[code] = struct{}{}
	return true
}

// RunCacheRepair retries failed tombstone writes until ctx is cancelled. A
// tombstone is always safe to write late, because deletion is final. Run it
// as a server worker when a cache is configured, and FlushCacheRepair as a
// shutdown hook: workers stop when shutdown begins, before in-flight
// deletes have finished.
func (s *Service) RunCacheRepair(ctx context.Context) error {
	if s.cache == nil {
		<-ctx.Done()
		return fmt.Errorf("link cache repair: %w", ctx.Err())
	}
	ticker := time.NewTicker(cacheRepairInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("link cache repair: %w", ctx.Err())
		case <-ticker.C:
			s.repairTombstones(ctx)
		}
	}
}

// FlushCacheRepair makes one last attempt at the queued tombstones. Run it
// after the listener has drained, so deletes handled during the drain are
// included. Tombstones still unwritten are logged as an error rather than
// returned: the cache is optional, and failing the shutdown would only turn
// a clean exit into a crash.
func (s *Service) FlushCacheRepair(ctx context.Context) {
	if s.cache == nil {
		return
	}
	if n := s.repairTombstones(ctx); n > 0 {
		s.log.ErrorContext(ctx, "link cache tombstones not written at shutdown; those deleted links may redirect until their cache TTL expires",
			"pending", n)
	}
}

// repairTombstones writes the queued tombstones until one fails, and
// returns how many are still queued.
func (s *Service) repairTombstones(ctx context.Context) int {
	s.mu.Lock()
	codes := make([]string, 0, len(s.pendingTombstones))
	for code := range s.pendingTombstones {
		codes = append(codes, code)
	}
	s.mu.Unlock()

	repaired := 0
	for _, code := range codes {
		if err := s.cache.Put(ctx, code, Destination{Deleted: true}); err != nil {
			// Still failing; the rest would too. Try again next tick.
			break
		}
		s.mu.Lock()
		delete(s.pendingTombstones, code)
		s.mu.Unlock()
		repaired++
	}
	if repaired > 0 {
		s.log.InfoContext(ctx, "link cache tombstones repaired", "repaired", repaired, "pending", len(codes)-repaired)
	}
	return s.pendingTombstoneCount()
}

func (s *Service) pendingTombstoneCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingTombstones)
}

// discardClicks is the default sink: it drops every click.
type discardClicks struct{}

func (discardClicks) Record(Click) {}
