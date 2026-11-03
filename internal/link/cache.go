package link

import (
	"context"
	"time"
)

// Destination is everything a redirect needs to know about a code. It is the
// unit the redirect cache stores, so it covers every answer a lookup can
// give: a live (possibly expired) link, a deleted one, or no link at all.
type Destination struct {
	LinkID    int64
	URL       string
	ExpiresAt *time.Time
	// Deleted marks a soft-deleted link. Deletion is final: codes are never
	// reused, so a Deleted entry can be written at any later time and still
	// be correct.
	Deleted bool
	// Missing marks a code no link has ever used: a negative cache entry.
	Missing bool
}

// Expired reports whether the destination's expiry has passed at time now.
func (d Destination) Expired(now time.Time) bool {
	return d.ExpiresAt != nil && !now.Before(*d.ExpiresAt)
}

func destinationOf(l Link) Destination {
	return Destination{
		LinkID:    l.ID,
		URL:       l.TargetURL,
		ExpiresAt: l.ExpiresAt,
		Deleted:   l.DeletedAt != nil,
	}
}

// Cache keeps redirect lookups off the database. The service treats every
// error as a miss and reads Postgres instead, so implementations must fail
// fast rather than retry. Methods must be safe for concurrent use.
//
// Coherence rests on two write modes. Readers Fill: after loading a
// destination from the database they store it only if the code has no
// entry yet. Writers Put: after creating or deleting a link they store the
// new state unconditionally. A Fill racing a Put therefore never overwrites
// the newer state, because the Put either lands after the Fill and replaces
// it, or lands first and makes the Fill a no-op.
type Cache interface {
	// Get returns the cached destination for code; ok is false on a miss.
	Get(ctx context.Context, code string) (d Destination, ok bool, err error)
	// Fill stores d for code unless the code already has an entry.
	Fill(ctx context.Context, code string, d Destination) error
	// Put stores d for code, replacing any entry.
	Put(ctx context.Context, code string, d Destination) error
}
