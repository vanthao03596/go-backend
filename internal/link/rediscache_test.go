package link

import (
	"errors"
	"testing"
	"time"
)

func TestCacheEntryRoundTrip(t *testing.T) {
	exp := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	for name, d := range map[string]Destination{
		"live":     {LinkID: 9, URL: "https://example.com/a?b=c"},
		"expiring": {LinkID: 9, URL: "https://example.com/a", ExpiresAt: &exp},
		"deleted":  {Deleted: true},
		"missing":  {Missing: true},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := encodeDestination(d)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeDestination(raw)
			if err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if got.LinkID != d.LinkID || got.URL != d.URL || got.Deleted != d.Deleted || got.Missing != d.Missing ||
				(got.ExpiresAt == nil) != (d.ExpiresAt == nil) || (d.ExpiresAt != nil && !got.ExpiresAt.Equal(*d.ExpiresAt)) {
				t.Errorf("round trip = %+v, want %+v", got, d)
			}
		})
	}
}

func TestDeletedEntriesDropTheTarget(t *testing.T) {
	// A tombstone does not need the target, and must not keep it around.
	raw, err := encodeDestination(Destination{LinkID: 9, URL: "https://example.com/secret", Deleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"deleted":true}` {
		t.Errorf("tombstone = %s", raw)
	}
}

func TestMalformedCacheEntriesAreRejected(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":        `nope`,
		"empty object":    `{}`,
		"id without url":  `{"id":3}`,
		"two shapes":      `{"missing":true,"deleted":true}`,
		"link and absent": `{"id":3,"url":"https://e.com","missing":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeDestination([]byte(raw)); !errors.Is(err, errBadCacheEntry) {
				t.Fatalf("decode %s = %v, want errBadCacheEntry", raw, err)
			}
		})
	}
	if _, err := encodeDestination(Destination{}); !errors.Is(err, errBadCacheEntry) {
		t.Errorf("encoding an empty destination = %v, want errBadCacheEntry", err)
	}
}
