package link

import (
	"errors"
	"strings"
	"testing"
)

func TestGenerateCode(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		code, err := GenerateCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != CodeLength {
			t.Fatalf("len(%q) = %d, want %d", code, len(code), CodeLength)
		}
		for _, c := range code {
			if !strings.ContainsRune(alphabet, c) {
				t.Fatalf("%q contains %q outside alphabet", code, c)
			}
		}
		if _, dup := seen[code]; dup {
			t.Fatalf("duplicate code %q in 1000 draws", code)
		}
		seen[code] = struct{}{}
	}
}

func TestValidateAlias(t *testing.T) {
	cases := map[string]error{
		"abc":          nil,
		"my-Link_01":   nil,
		"ab":           ErrInvalidAlias,
		"has space":    ErrInvalidAlias,
		"with/slash":   ErrInvalidAlias,
		"api":          ErrReservedAlias,
		"HEALTHZ":      ErrReservedAlias,
		"openapi.json": ErrInvalidAlias, // dot fails the pattern before the reserved check
	}
	for alias, want := range cases {
		err := ValidateAlias(alias)
		if !errors.Is(err, want) {
			t.Errorf("ValidateAlias(%q) = %v, want %v", alias, err, want)
		}
	}
	long := strings.Repeat("a", 33)
	if !errors.Is(ValidateAlias(long), ErrInvalidAlias) {
		t.Error("33-char alias accepted")
	}
}

func TestValidateTargetURL(t *testing.T) {
	ok := []string{
		"https://example.com",
		"http://example.com/path?q=1#frag",
		"https://sub.example.co.uk:8443/x",
		"https://93.184.216.34/",
		"https://[2606:4700::6810:84e5]/",
		"https://xn--bcher-kva.example/",
		"https://example.com./trailing-dot",
	}
	for _, u := range ok {
		if err := ValidateTargetURL(u); err != nil {
			t.Errorf("ValidateTargetURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"",
		"example.com",
		"ftp://example.com",
		"javascript:alert(1)",
		"https://",
		"https://localhost/",
		"https://foo.localhost/",
		"https://intranet/",
		"https://svc.internal/",
		"https://127.0.0.1/",
		"https://10.1.2.3/",
		"https://[::1]/",
		"https://169.254.169.254/latest/meta-data",
		"https://127.1/",
		"https://0x7f.0.0.1/",
		"https://0177.0.0.1/",
		"https://100.64.1.1/",
		"https://[::ffff:127.0.0.1]/",
		"https://[64:ff9b::7f00:1]/",
		"https://0.0.0.0/",
		"https://printer.local/",
		"https://user:pw@example.com/",
		"https://example.com/" + strings.Repeat("a", MaxURLLength),
	}
	for _, u := range bad {
		if err := ValidateTargetURL(u); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("ValidateTargetURL(%q) = %v, want ErrInvalidURL", u, err)
		}
	}
}
