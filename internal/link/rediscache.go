package link

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache TTL defaults, matching config's LINK_CACHE_TTL and
// LINK_CACHE_NEGATIVE_TTL defaults.
const (
	DefaultCacheTTL         = 10 * time.Minute
	DefaultNegativeCacheTTL = time.Minute
)

// cacheKeyPrefix namespaces the entries. The version changes with the value
// format so a rolling deploy never decodes the other format.
const cacheKeyPrefix = "shortener:link:v1:"

// RedisCache is the Cache backed by Redis. Each code is one string key
// holding a small JSON document, with a TTL: ttl for links, negativeTTL for
// codes that do not exist.
type RedisCache struct {
	rdb         redis.Cmdable
	ttl         time.Duration
	negativeTTL time.Duration
}

var _ Cache = (*RedisCache)(nil)

// NewRedisCache stores entries in rdb. A TTL that is not positive gets its
// default: Redis reads an expiry of zero as "never", and an entry that never
// expires would turn a single failed invalidation into a permanent one.
func NewRedisCache(rdb redis.Cmdable, ttl, negativeTTL time.Duration) *RedisCache {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	if negativeTTL <= 0 {
		negativeTTL = DefaultNegativeCacheTTL
	}
	return &RedisCache{rdb: rdb, ttl: ttl, negativeTTL: negativeTTL}
}

// Get implements Cache.
func (c *RedisCache) Get(ctx context.Context, code string) (Destination, bool, error) {
	raw, err := c.rdb.Get(ctx, cacheKeyPrefix+code).Bytes()
	if errors.Is(err, redis.Nil) {
		return Destination{}, false, nil
	}
	if err != nil {
		return Destination{}, false, fmt.Errorf("redis get: %w", err)
	}
	d, err := decodeDestination(raw)
	if err != nil {
		return Destination{}, false, err
	}
	return d, true, nil
}

// Fill implements Cache with SET NX.
func (c *RedisCache) Fill(ctx context.Context, code string, d Destination) error {
	raw, err := encodeDestination(d)
	if err != nil {
		return err
	}
	if err := c.rdb.SetNX(ctx, cacheKeyPrefix+code, raw, c.ttlFor(d)).Err(); err != nil {
		return fmt.Errorf("redis set nx: %w", err)
	}
	return nil
}

// Put implements Cache with a plain SET.
func (c *RedisCache) Put(ctx context.Context, code string, d Destination) error {
	raw, err := encodeDestination(d)
	if err != nil {
		return err
	}
	if err := c.rdb.Set(ctx, cacheKeyPrefix+code, raw, c.ttlFor(d)).Err(); err != nil {
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

func (c *RedisCache) ttlFor(d Destination) time.Duration {
	if d.Missing {
		return c.negativeTTL
	}
	return c.ttl
}

// cacheEntry is the stored form of a Destination. Exactly one shape is
// valid: missing, deleted, or a link with an id and a URL. Expiry is stored
// rather than folded into the TTL, so an expired link keeps answering 410
// from the cache.
type cacheEntry struct {
	ID        int64      `json:"id,omitzero"`
	URL       string     `json:"url,omitzero"`
	ExpiresAt *time.Time `json:"exp,omitzero"`
	Deleted   bool       `json:"deleted,omitzero"`
	Missing   bool       `json:"missing,omitzero"`
}

var errBadCacheEntry = errors.New("malformed link cache entry")

func encodeDestination(d Destination) ([]byte, error) {
	e := cacheEntry{Missing: d.Missing, Deleted: d.Deleted}
	if !d.Missing && !d.Deleted {
		e.ID, e.URL, e.ExpiresAt = d.LinkID, d.URL, d.ExpiresAt
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("encode link cache entry: %w", err)
	}
	return raw, nil
}

func decodeDestination(raw []byte) (Destination, error) {
	var e cacheEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return Destination{}, fmt.Errorf("%w: %w", errBadCacheEntry, err)
	}
	if err := e.validate(); err != nil {
		return Destination{}, err
	}
	return Destination{
		LinkID:    e.ID,
		URL:       e.URL,
		ExpiresAt: e.ExpiresAt,
		Deleted:   e.Deleted,
		Missing:   e.Missing,
	}, nil
}

func (e cacheEntry) validate() error {
	live := e.ID > 0 && e.URL != ""
	n := 0
	for _, b := range []bool{e.Missing, e.Deleted, live} {
		if b {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("%w: want exactly one of missing, deleted or id and url", errBadCacheEntry)
	}
	return nil
}
