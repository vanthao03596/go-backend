package redisx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestParseURLAppliesCacheDefaults(t *testing.T) {
	o, err := ParseURL("redis://:secret@cache.internal:6380/2")
	if err != nil {
		t.Fatal(err)
	}
	if o.Addr != "cache.internal:6380" || o.DB != 2 || o.Password != "secret" {
		t.Errorf("addr/db/password = %s/%d/%q", o.Addr, o.DB, o.Password)
	}
	if o.DialTimeout != DefaultDialTimeout || o.ReadTimeout != DefaultReadTimeout || o.PoolTimeout != DefaultPoolTimeout {
		t.Errorf("timeouts = dial %s read %s pool %s", o.DialTimeout, o.ReadTimeout, o.PoolTimeout)
	}
	// WriteTimeout stays unset so go-redis copies ReadTimeout into it.
	if o.WriteTimeout != 0 || o.MaxRetries != -1 || o.DialerRetries != 1 {
		t.Errorf("write timeout %s, max retries %d, dialer retries %d", o.WriteTimeout, o.MaxRetries, o.DialerRetries)
	}
}

func TestParseURLKeepsExplicitSettings(t *testing.T) {
	o, err := ParseURL("redis://localhost/0?read_timeout=250ms&dial_timeout=1s&pool_timeout=0&max_retries=2")
	if err != nil {
		t.Fatal(err)
	}
	// go-redis maps an explicit 0 to -1, "no timeout"; it must survive.
	if o.ReadTimeout != 250*time.Millisecond || o.DialTimeout != time.Second || o.PoolTimeout != -1 || o.MaxRetries != 2 {
		t.Errorf("read %s dial %s pool %s retries %d", o.ReadTimeout, o.DialTimeout, o.PoolTimeout, o.MaxRetries)
	}
}

func TestParseURLErrorsDoNotLeakThePassword(t *testing.T) {
	for _, bad := range []string{
		"redis://:hunter2@host:6379/0?bogus=1",
		"redis://:hunter2@host:6379/0\x7f",
		"redis://:hunter2@[::1",
	} {
		_, err := ParseURL(bad)
		if err == nil {
			t.Fatalf("ParseURL(%q) succeeded", bad)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}

// opErr is a transport failure as go-redis reports it.
var opErr = &net.OpError{Op: "read", Net: "tcp", Err: errors.New("i/o timeout")}

func newTestBreaker() *Breaker {
	return NewBreaker(BreakerConfig{Threshold: 3, Cooldown: time.Second}, slog.New(slog.DiscardHandler))
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestBreaker()

		// Successes in between reset the count.
		for range 5 {
			b.ReportResult(opErr)
			b.ReportResult(opErr)
			b.ReportResult(nil)
		}
		if b.Open() {
			t.Fatal("opened without three consecutive failures")
		}

		for range 3 {
			if err := b.Allow(); err != nil {
				t.Fatalf("Allow while closed = %v", err)
			}
			b.ReportResult(opErr)
		}
		if !b.Open() {
			t.Fatal("still closed after three consecutive failures")
		}
		if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("Allow while open = %v, want ErrCircuitOpen", err)
		}
	})
}

func TestBreakerProbesOnceAfterCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newTestBreaker()
		for range 3 {
			b.ReportResult(opErr)
		}

		time.Sleep(time.Second)
		if err := b.Allow(); err != nil {
			t.Fatalf("probe refused after cooldown: %v", err)
		}
		// Only one probe at a time.
		if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("second caller during the probe = %v, want ErrCircuitOpen", err)
		}

		// A failed probe re-opens for another cooldown.
		b.ReportResult(opErr)
		time.Sleep(time.Second / 2)
		if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("Allow half a cooldown after a failed probe = %v", err)
		}

		// A successful probe closes it.
		time.Sleep(time.Second)
		if err := b.Allow(); err != nil {
			t.Fatalf("second probe refused: %v", err)
		}
		b.ReportResult(nil)
		if b.Open() {
			t.Fatal("still open after a successful probe")
		}
		for range 10 {
			if err := b.Allow(); err != nil {
				t.Fatalf("Allow after closing = %v", err)
			}
		}
	})
}

// replyErr is an error reply from the server, as go-redis reports one.
type replyErr string

func (e replyErr) Error() string { return string(e) }
func (replyErr) RedisError()     {}

var _ redis.Error = replyErr("")

func TestBreakerCountsMissesAsSuccess(t *testing.T) {
	b := newTestBreaker()
	for range 10 {
		b.ReportResult(opErr)
		b.ReportResult(opErr)
		b.ReportResult(fmt.Errorf("redis get: %w", redis.Nil)) // a cache miss, wrapped as RedisCache does
		b.ReportResult(context.Canceled)                       // ignored
		b.ReportResult(context.DeadlineExceeded)               // ignored
	}
	if b.Open() {
		t.Fatal("opened although misses kept arriving between failures")
	}
}

// TestBreakerOpensOnErrorReplies covers a server that answers but cannot
// serve: a wrong password, a read-only replica after failover, out of
// memory. Every call is a wasted round trip, so it must open like a timeout.
func TestBreakerOpensOnErrorReplies(t *testing.T) {
	for _, reply := range []replyErr{
		"WRONGPASS invalid username-password pair or user is disabled.",
		"READONLY You can't write against a read only replica.",
		"OOM command not allowed when used memory > 'maxmemory'.",
	} {
		b := newTestBreaker()
		for range 3 {
			b.ReportResult(fmt.Errorf("redis set: %w", reply))
		}
		if !b.Open() {
			t.Errorf("still closed after three %q replies", reply)
		}
	}
}

// TestHungServerFailsFast is the reason the breaker exists: a server that
// accepts connections and never answers costs one read timeout per caller
// until the circuit opens, then nothing.
func TestHungServerFailsFast(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			held = append(held, c) // never read, never reply
		}
	}()

	rdb, err := NewClient("redis://"+lis.Addr().String()+"/0?read_timeout=50ms", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	b := NewBreaker(BreakerConfig{}, slog.New(slog.DiscardHandler))

	start := time.Now()
	var refused int
	for i := range 20 {
		err := b.Allow()
		if err == nil {
			err = rdb.Get(t.Context(), "k").Err()
			b.ReportResult(err)
		}
		if err == nil || errors.Is(err, redis.Nil) {
			t.Fatalf("GET %d against a hung server = %v", i, err)
		}
		if errors.Is(err, ErrCircuitOpen) {
			refused++
		}
	}
	elapsed := time.Since(start)
	if refused != 20-DefaultBreakerThreshold {
		t.Errorf("%d of 20 calls short-circuited, want %d", refused, 20-DefaultBreakerThreshold)
	}
	// Without the breaker this is 20 x 50ms.
	if elapsed > 750*time.Millisecond {
		t.Errorf("20 calls took %s", elapsed)
	}
}
