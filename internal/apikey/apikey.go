// Package apikey issues and authenticates the bearer keys that guard the
// management API. A raw key is shown to the operator exactly once; only its
// SHA-256 hash is stored, so a database leak does not leak usable credentials.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// Prefix marks raw keys so they are recognisable in logs and secret
	// scanners without revealing anything about the value.
	Prefix = "sk_"
	// rawEntropyBytes is the random payload of a key: 256 bits.
	rawEntropyBytes = 32
	// MaxRawLen bounds what the middleware is willing to hash. A valid key is
	// 46 characters; anything far longer is noise, not a credential.
	MaxRawLen = 128
	// MaxNameLen caps the human label stored with a key.
	MaxNameLen = 64
)

// Key is an issued API key. It never carries the raw secret.
type Key struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// Domain errors.
var (
	ErrNotFound    = errors.New("api key not found or revoked")
	ErrInvalidName = errors.New("api key name must be 1 to 64 characters")
)

// Repository is the persistence boundary the service depends on.
type Repository interface {
	// Insert stores a new key hash and returns the created row.
	Insert(ctx context.Context, keyHash []byte, name string) (Key, error)
	// GetByHash returns the active key with this hash, or ErrNotFound.
	GetByHash(ctx context.Context, keyHash []byte) (Key, error)
}

// Service holds the business rules for keys. It knows nothing about HTTP or SQL.
type Service struct {
	repo Repository
}

// NewService wires a Service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create issues a new key labelled name. It returns the stored row and the raw
// secret. The secret cannot be recovered afterwards.
func (s *Service) Create(ctx context.Context, name string) (Key, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxNameLen {
		return Key{}, "", ErrInvalidName
	}
	raw, err := Generate()
	if err != nil {
		return Key{}, "", err
	}
	k, err := s.repo.Insert(ctx, Hash(raw), name)
	if err != nil {
		return Key{}, "", err
	}
	return k, raw, nil
}

// Authenticate resolves a raw bearer token to its key. It returns ErrNotFound
// for unknown, revoked or malformed tokens so callers cannot distinguish them.
func (s *Service) Authenticate(ctx context.Context, raw string) (Key, error) {
	if !looksLikeKey(raw) {
		return Key{}, ErrNotFound
	}
	return s.repo.GetByHash(ctx, Hash(raw))
}

// Generate returns a fresh raw key: the prefix followed by 256 random bits in
// unpadded base64url.
func Generate() (string, error) {
	buf := make([]byte, rawEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Hash returns the SHA-256 digest used as the lookup key in storage. Keys are
// high-entropy random strings, so a fast unsalted hash is sufficient and a
// constant-time compare is unnecessary: the hash itself is the index lookup.
func Hash(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// looksLikeKey rejects obviously wrong input before touching the database.
func looksLikeKey(raw string) bool {
	return strings.HasPrefix(raw, Prefix) && len(raw) > len(Prefix) && len(raw) <= MaxRawLen
}
