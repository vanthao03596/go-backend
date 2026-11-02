// Package link is the bounded context for short links: creation, lookup,
// listing, deletion and redirect resolution.
package link

import (
	"context"
	"errors"
	"time"
)

// Link is the domain representation of a short link.
type Link struct {
	ID         int64
	Code       string
	TargetURL  string
	APIKeyID   int64
	ClickCount int64
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	DeletedAt  *time.Time
}

// Expired reports whether the link's expiry has passed at time now.
func (l Link) Expired(now time.Time) bool {
	return l.ExpiresAt != nil && !now.Before(*l.ExpiresAt)
}

// CreateParams are the caller-supplied inputs for a new link.
type CreateParams struct {
	URL       string
	Alias     string // optional custom code
	ExpiresAt *time.Time
}

// Cursor identifies a position in a caller's link list ordered by
// (created_at DESC, id DESC).
type Cursor struct {
	CreatedAt time.Time
	ID        int64
}

// Domain errors. Handlers map these to HTTP statuses.
var (
	ErrNotFound      = errors.New("link not found")
	ErrGone          = errors.New("link expired or deleted")
	ErrCodeTaken     = errors.New("code already in use")
	ErrInvalidURL    = errors.New("invalid target url")
	ErrInvalidAlias  = errors.New("invalid alias")
	ErrReservedAlias = errors.New("alias is reserved")
	ErrInvalidExpiry = errors.New("expires_at must be in the future")
	ErrInvalidCursor = errors.New("invalid cursor")
)

// Repository is the persistence boundary the service depends on.
type Repository interface {
	// Insert stores a new link. It returns ErrCodeTaken on a code collision.
	Insert(ctx context.Context, code, targetURL string, apiKeyID int64, expiresAt *time.Time) (Link, error)
	// GetByCode returns a link regardless of deletion state, or ErrNotFound.
	GetByCode(ctx context.Context, code string) (Link, error)
	// GetByCodeForKey returns a live link owned by apiKeyID, or ErrNotFound.
	GetByCodeForKey(ctx context.Context, code string, apiKeyID int64) (Link, error)
	// List returns up to limit live links owned by apiKeyID before cursor.
	List(ctx context.Context, apiKeyID int64, cursor Cursor, limit int32) ([]Link, error)
	// SoftDelete marks a live link deleted. It returns ErrNotFound if nothing matched.
	SoftDelete(ctx context.Context, code string, apiKeyID int64) error
	// IncrementClicks adds delta to the link's click counter.
	IncrementClicks(ctx context.Context, id, delta int64) error
}
