package link

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math"
	"time"
)

// Pagination cursors are signed so a client can only hand back a cursor the
// service issued, for the key it was issued to. Layout before base64url:
//
//	version (1) | created_at unix nanos (8) | id (8) | tag (16)
//
// tag is HMAC-SHA256 over a context string, the version, the caller's API
// key id and the position, truncated to 128 bits (RFC 2104 section 5
// recommends keeping at least half the hash output). Binding the key id
// means a cursor leaked from one caller is useless to another. The position
// is signed, not encrypted: a client that decodes the cursor learns the
// row's created_at, which the API already returns, and its internal id.
const (
	cursorVersion = 1
	cursorTagLen  = 16
	cursorLen     = 1 + 8 + 8 + cursorTagLen
	// cursorContext separates this MAC from any other use of the secret.
	cursorContext = "shortener/link-list-cursor"
)

// cursorSigner encodes and verifies list cursors under one secret.
type cursorSigner struct {
	secret []byte
}

// newCursorSigner returns a signer for secret. An empty secret gets a random
// one, valid for the life of the process.
func newCursorSigner(secret []byte) cursorSigner {
	if len(secret) == 0 {
		secret = make([]byte, 32)
		_, _ = rand.Read(secret) // crypto/rand.Read never fails since Go 1.24
	}
	return cursorSigner{secret: secret}
}

func (s cursorSigner) encode(apiKeyID int64, c Cursor) string {
	buf := make([]byte, 0, cursorLen)
	buf = append(buf, cursorVersion)
	buf = binary.BigEndian.AppendUint64(buf, uint64(c.CreatedAt.UnixNano()))
	buf = binary.BigEndian.AppendUint64(buf, uint64(c.ID)) //nolint:gosec // G115: the bit pattern round-trips
	buf = append(buf, s.tag(apiKeyID, buf)...)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// decode verifies a cursor issued to apiKeyID. The empty string is the
// first page.
func (s cursorSigner) decode(apiKeyID int64, encoded string) (Cursor, error) {
	if encoded == "" {
		return Cursor{CreatedAt: time.Unix(0, math.MaxInt64), ID: math.MaxInt64}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != cursorLen || raw[0] != cursorVersion {
		return Cursor{}, ErrInvalidCursor
	}
	body, tag := raw[:cursorLen-cursorTagLen], raw[cursorLen-cursorTagLen:]
	if !hmac.Equal(tag, s.tag(apiKeyID, body)) {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{
		CreatedAt: time.Unix(0, int64(binary.BigEndian.Uint64(body[1:9]))), //nolint:gosec // G115: the bit pattern round-trips
		ID:        int64(binary.BigEndian.Uint64(body[9:17])),              //nolint:gosec // G115: the bit pattern round-trips
	}, nil
}

func (s cursorSigner) tag(apiKeyID int64, body []byte) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(cursorContext))
	mac.Write(binary.BigEndian.AppendUint64(nil, uint64(apiKeyID))) //nolint:gosec // G115: the bit pattern round-trips
	mac.Write(body)
	return mac.Sum(nil)[:cursorTagLen]
}
