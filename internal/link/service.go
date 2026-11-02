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
)

const (
	// maxCodeAttempts bounds retries when a generated code collides.
	maxCodeAttempts = 3
	// DefaultPageSize is used when the caller omits a limit.
	DefaultPageSize int32 = 20
	// MaxPageSize caps the caller-supplied limit.
	MaxPageSize int32 = 100
)

// Service holds the business rules. It knows nothing about HTTP or SQL.
type Service struct {
	repo    Repository
	baseURL string
	log     *slog.Logger
	now     func() time.Time
}

// NewService wires a Service. baseURL is the public origin used to build short URLs.
func NewService(repo Repository, baseURL string, log *slog.Logger) *Service {
	return &Service{
		repo:    repo,
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		now:     time.Now,
	}
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
		return s.repo.Insert(ctx, p.Alias, p.URL, apiKeyID, p.ExpiresAt)
	}

	for attempt := 1; attempt <= maxCodeAttempts; attempt++ {
		code, err := GenerateCode()
		if err != nil {
			return Link{}, err
		}
		l, err := s.repo.Insert(ctx, code, p.URL, apiKeyID, p.ExpiresAt)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, ErrCodeTaken) {
			return Link{}, err
		}
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

// clickWriteTimeout bounds the detached click increment.
const clickWriteTimeout = 2 * time.Second

// Resolve returns the target URL for a redirect. When countClick is true the
// link's counter is incremented; callers pass false for HEAD requests so
// previewers and health checks do not inflate the count.
// It returns ErrNotFound for unknown codes and ErrGone for deleted or expired links.
func (s *Service) Resolve(ctx context.Context, code string, countClick bool) (string, error) {
	l, err := s.repo.GetByCode(ctx, code)
	if err != nil {
		return "", err
	}
	if l.DeletedAt != nil || l.Expired(s.now()) {
		return "", ErrGone
	}
	if countClick {
		// Synchronous for now (roadmap phase 5 moves this off the hot path).
		// The write is detached from the request context so a client that
		// disconnects right after the 302 cannot cancel it, and a failed
		// increment must not break the redirect.
		incCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clickWriteTimeout)
		defer cancel()
		if err := s.repo.IncrementClicks(incCtx, l.ID, 1); err != nil {
			s.log.ErrorContext(ctx, "increment clicks", "code", code, "err", err)
		}
	}
	return l.TargetURL, nil
}

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
