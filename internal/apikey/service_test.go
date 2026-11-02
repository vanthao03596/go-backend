package apikey

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeRepo struct {
	byHash map[string]Key
	nextID int64
	err    error
}

func newFakeRepo() *fakeRepo { return &fakeRepo{byHash: map[string]Key{}} }

func (f *fakeRepo) Insert(_ context.Context, keyHash []byte, name string) (Key, error) {
	if f.err != nil {
		return Key{}, f.err
	}
	f.nextID++
	k := Key{ID: f.nextID, Name: name, CreatedAt: time.Now()}
	f.byHash[string(keyHash)] = k
	return k, nil
}

func (f *fakeRepo) GetByHash(_ context.Context, keyHash []byte) (Key, error) {
	if f.err != nil {
		return Key{}, f.err
	}
	k, ok := f.byHash[string(keyHash)]
	if !ok {
		return Key{}, ErrNotFound
	}
	return k, nil
}

func TestGenerateFormat(t *testing.T) {
	a, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Generate()
	if a == b {
		t.Fatal("two generated keys are identical")
	}
	if !strings.HasPrefix(a, Prefix) {
		t.Errorf("key %q lacks prefix %q", a, Prefix)
	}
	// 3 prefix chars + 43 chars of unpadded base64url for 32 bytes.
	if len(a) != 46 {
		t.Errorf("len = %d, want 46", len(a))
	}
}

func TestHashIsDeterministicAndDoesNotStoreRaw(t *testing.T) {
	raw := "sk_example"
	first, second := Hash(raw), Hash(raw)
	if string(first) != string(second) {
		t.Fatal("hash is not deterministic")
	}
	if len(Hash(raw)) != 32 {
		t.Errorf("hash len = %d, want 32", len(Hash(raw)))
	}
	if strings.Contains(string(Hash(raw)), "example") {
		t.Error("hash contains raw key material")
	}
}

func TestCreateThenAuthenticate(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	k, raw, err := svc.Create(t.Context(), "  ci-bot  ")
	if err != nil {
		t.Fatal(err)
	}
	if k.Name != "ci-bot" {
		t.Errorf("name = %q, want trimmed", k.Name)
	}
	got, err := svc.Authenticate(t.Context(), raw)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != k.ID {
		t.Errorf("id = %d, want %d", got.ID, k.ID)
	}
}

func TestCreateRejectsBadNames(t *testing.T) {
	svc := NewService(newFakeRepo())
	for _, name := range []string{"", "   ", strings.Repeat("x", MaxNameLen+1)} {
		if _, _, err := svc.Create(t.Context(), name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: err = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestAuthenticateRejectsWithoutDBRoundTrip(t *testing.T) {
	repo := newFakeRepo()
	repo.err = errors.New("db should not be called")
	svc := NewService(repo)

	for _, raw := range []string{"", "sk_", "notaprefix", strings.Repeat("sk_", MaxRawLen)} {
		if _, err := svc.Authenticate(t.Context(), raw); !errors.Is(err, ErrNotFound) {
			t.Errorf("raw %q: err = %v, want ErrNotFound", raw, err)
		}
	}
}

func TestAuthenticateUnknownKey(t *testing.T) {
	svc := NewService(newFakeRepo())
	raw, _ := Generate()
	if _, err := svc.Authenticate(t.Context(), raw); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
