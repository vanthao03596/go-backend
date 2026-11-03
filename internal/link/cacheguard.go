package link

import (
	"context"
	"errors"
)

// CircuitBreaker decides whether a cache call may proceed and learns how it
// went. redisx.Breaker implements it.
type CircuitBreaker interface {
	// Allow returns nil if the call may proceed, or the error to fail it
	// with at once.
	Allow() error
	// ReportResult records the outcome of an allowed call.
	ReportResult(err error)
}

// GuardCache returns c with every call passed through b. While b is open the
// cache fails at once and the service reads the database, so a hung cache
// costs one probe per cooldown instead of a timeout per redirect.
func GuardCache(c Cache, b CircuitBreaker) Cache {
	return guardedCache{c: c, b: b}
}

type guardedCache struct {
	c Cache
	b CircuitBreaker
}

func (g guardedCache) Get(ctx context.Context, code string) (Destination, bool, error) {
	if err := g.b.Allow(); err != nil {
		return Destination{}, false, err
	}
	d, ok, err := g.c.Get(ctx, code)
	if errors.Is(err, errBadCacheEntry) {
		// The server answered; the entry is what is wrong.
		g.b.ReportResult(nil)
	} else {
		g.b.ReportResult(err)
	}
	return d, ok, err
}

func (g guardedCache) Fill(ctx context.Context, code string, d Destination) error {
	if err := g.b.Allow(); err != nil {
		return err
	}
	err := g.c.Fill(ctx, code, d)
	g.b.ReportResult(err)
	return err
}

func (g guardedCache) Put(ctx context.Context, code string, d Destination) error {
	if err := g.b.Allow(); err != nil {
		return err
	}
	err := g.c.Put(ctx, code, d)
	g.b.ReportResult(err)
	return err
}
