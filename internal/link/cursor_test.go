package link

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	s := newCursorSigner([]byte(strings.Repeat("k", 32)))
	want := Cursor{CreatedAt: time.Date(2026, 11, 3, 10, 0, 0, 123456000, time.UTC), ID: 42}

	enc := s.encode(7, want)
	if len(enc) != 44 {
		t.Errorf("cursor %q has length %d, want 44", enc, len(enc))
	}
	got, err := s.decode(7, enc)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || got.ID != want.ID {
		t.Errorf("decode = %+v, want %+v", got, want)
	}

	first, err := s.decode(7, "")
	if err != nil || first.ID <= want.ID || !first.CreatedAt.After(want.CreatedAt) {
		t.Errorf("empty cursor = %+v, %v; want a position after every row", first, err)
	}
}

func TestCursorRejectsAnythingNotIssuedToTheCaller(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	s := newCursorSigner(secret)
	valid := s.encode(7, Cursor{CreatedAt: time.Unix(1_800_000_000, 0), ID: 42})
	raw, _ := base64.RawURLEncoding.DecodeString(valid)

	tamper := func(i int) string {
		b := []byte(string(raw))
		b[i] ^= 0x01
		return base64.RawURLEncoding.EncodeToString(b)
	}
	cases := map[string]struct {
		signer cursorSigner
		key    int64
		cursor string
	}{
		"other api key":    {s, 8, valid},
		"other secret":     {newCursorSigner([]byte(strings.Repeat("x", 32))), 7, valid},
		"position edited":  {s, 7, tamper(9)},
		"timestamp edited": {s, 7, tamper(3)},
		"tag edited":       {s, 7, tamper(len(raw) - 1)},
		"version edited":   {s, 7, tamper(0)},
		"truncated":        {s, 7, valid[:len(valid)-2]},
		"not base64":       {s, 7, "!!not-base64!!"},
		// The unsigned phase 2 format: base64url("<unix nanos>:<id>").
		"legacy unsigned": {s, 7, base64.RawURLEncoding.EncodeToString([]byte("1800000000000000000:42"))},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tc.signer.decode(tc.key, tc.cursor); !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("decode = %v, want ErrInvalidCursor", err)
			}
		})
	}
}

func TestCursorWithoutSecretIsRandomPerSigner(t *testing.T) {
	a, b := newCursorSigner(nil), newCursorSigner(nil)
	c := a.encode(1, Cursor{CreatedAt: time.Unix(1, 0), ID: 1})
	if _, err := a.decode(1, c); err != nil {
		t.Fatalf("own cursor rejected: %v", err)
	}
	if _, err := b.decode(1, c); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cursor accepted by a signer with another random key: %v", err)
	}
}

func TestListCursorIsBoundToTheCaller(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo, WithCursorSecret([]byte(strings.Repeat("k", 32))))
	for range 3 {
		if _, err := svc.Create(t.Context(), 1, CreateParams{URL: "https://example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	_, next, err := svc.List(t.Context(), 1, "", 2)
	if err != nil || next == "" {
		t.Fatalf("page 1: next=%q err=%v", next, err)
	}
	if _, _, err := svc.List(t.Context(), 2, next, 2); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("another key's cursor = %v, want ErrInvalidCursor", err)
	}

	// A service sharing the secret (another replica) accepts it.
	replica := newTestService(repo, WithCursorSecret([]byte(strings.Repeat("k", 32))))
	if rows, _, err := replica.List(t.Context(), 1, next, 2); err != nil || len(rows) != 1 {
		t.Errorf("replica page 2: n=%d err=%v", len(rows), err)
	}
}
