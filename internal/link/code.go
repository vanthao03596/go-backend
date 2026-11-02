package link

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

const (
	// CodeLength is the length of generated codes. 62^7 is about 3.5e12.
	CodeLength = 7
	alphabet   = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	// Largest multiple of len(alphabet) below 256. Bytes at or above this
	// are rejected so every character is drawn uniformly.
	rejectAbove = 248
)

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

// reserved lists first path segments the router owns. Aliases must not shadow them.
var reserved = map[string]struct{}{
	"api": {}, "healthz": {}, "readyz": {}, "docs": {}, "openapi.json": {},
	"openapi.yaml": {}, "schemas": {}, "metrics": {}, "favicon.ico": {}, "robots.txt": {},
}

// GenerateCode returns a random base62 code of CodeLength characters.
func GenerateCode() (string, error) {
	var sb strings.Builder
	sb.Grow(CodeLength)
	buf := make([]byte, CodeLength*2)
	for sb.Len() < CodeLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("read random bytes: %w", err)
		}
		for _, b := range buf {
			if b >= rejectAbove {
				continue
			}
			sb.WriteByte(alphabet[int(b)%len(alphabet)])
			if sb.Len() == CodeLength {
				break
			}
		}
	}
	return sb.String(), nil
}

// ValidateAlias checks a caller-supplied code against the format and reserved list.
func ValidateAlias(alias string) error {
	if !aliasPattern.MatchString(alias) {
		return fmt.Errorf("%w: must match %s", ErrInvalidAlias, aliasPattern.String())
	}
	if _, bad := reserved[strings.ToLower(alias)]; bad {
		return fmt.Errorf("%w: %q", ErrReservedAlias, alias)
	}
	return nil
}
