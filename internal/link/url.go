package link

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// MaxURLLength mirrors the CHECK constraint on links.target_url.
const MaxURLLength = 2048

// Address ranges that are never a valid public redirect target beyond what
// netip.Addr's own predicates cover.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this" network
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64
	netip.MustParsePrefix("2001:db8::/32"), // documentation
}

// ValidateTargetURL accepts absolute http(s) URLs with a public-looking host.
// It does not resolve DNS; the service only redirects to the URL, it never
// fetches it, so the check is a guard against obviously bad input rather
// than a full SSRF defence.
func ValidateTargetURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: empty", ErrInvalidURL)
	}
	if len(raw) > MaxURLLength {
		return fmt.Errorf("%w: longer than %d characters", ErrInvalidURL, MaxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme must be http or https", ErrInvalidURL)
	}
	if u.User != nil {
		return fmt.Errorf("%w: credentials in url are not allowed", ErrInvalidURL)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: missing host", ErrInvalidURL)
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		return validateIP(addr)
	}
	return validateHostname(host)
}

func validateIP(addr netip.Addr) error {
	addr = addr.Unmap() // ::ffff:127.0.0.1 -> 127.0.0.1
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return fmt.Errorf("%w: non-public ip address", ErrInvalidURL)
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return fmt.Errorf("%w: non-public ip address", ErrInvalidURL)
		}
	}
	return nil
}

func validateHostname(host string) error {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	labels := strings.Split(lower, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%w: host must be a public domain", ErrInvalidURL)
	}
	switch {
	case lower == "localhost", strings.HasSuffix(lower, ".localhost"),
		strings.HasSuffix(lower, ".internal"), strings.HasSuffix(lower, ".local"):
		return fmt.Errorf("%w: host must be a public domain", ErrInvalidURL)
	}
	// Public TLDs are alphabetic (or punycode "xn--"). A numeric or hex last
	// label means an IPv4 literal in a shorthand form that netip does not
	// parse, such as 127.1 or 0x7f.0.0.1, which browsers still resolve.
	tld := labels[len(labels)-1]
	if tld == "" || tld[0] < 'a' || tld[0] > 'z' {
		return fmt.Errorf("%w: host must be a public domain", ErrInvalidURL)
	}
	return nil
}
