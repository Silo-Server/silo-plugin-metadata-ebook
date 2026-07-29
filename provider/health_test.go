package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

type healthClock struct {
	now time.Time
}

func (c *healthClock) Now() time.Time {
	return c.now
}

func (c *healthClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

type sourceHTTPError struct {
	status     int
	retryAfter time.Duration
}

func (e sourceHTTPError) Error() string {
	return fmt.Sprintf("source request: status %d", e.status)
}

func (e sourceHTTPError) HTTPStatusCode() int {
	return e.status
}

func (e sourceHTTPError) RetryAfter() time.Duration {
	return e.retryAfter
}

func TestSourceHealthPolicyFailuresCoolDownForOneHour(t *testing.T) {
	for _, status := range []int{403, 405} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			clock := &healthClock{now: time.Unix(1000, 0)}
			health := newSourceHealth(clock.Now)

			health.RecordFailure("catalog", sourceHTTPError{status: status})

			if health.Allow("catalog") {
				t.Fatalf("Allow() = true immediately after HTTP %d", status)
			}
			clock.Advance(time.Hour - time.Nanosecond)
			if health.Allow("catalog") {
				t.Fatalf("Allow() = true before one-hour cooldown elapsed")
			}
			clock.Advance(time.Nanosecond)
			if !health.Allow("catalog") {
				t.Fatalf("Allow() = false after one-hour cooldown elapsed")
			}
		})
	}
}

func TestSourceHealthZeroValueIsUsable(t *testing.T) {
	var health SourceHealth

	health.RecordFailure("catalog", sourceHTTPError{status: 403})

	if health.Allow("catalog") {
		t.Fatal("Allow() = true after zero-value health recorded HTTP 403")
	}
}

func TestSourceHealthDoesNotParseStatusFromPlainErrorText(t *testing.T) {
	clock := &healthClock{now: time.Unix(1000, 0)}
	health := newSourceHealth(clock.Now)

	health.RecordFailure("catalog", errors.New("GET https://example.test: status 405"))

	if health.Allow("catalog") {
		t.Fatal("Allow() = true before generic cooldown elapsed")
	}
	clock.Advance(healthTimeoutBase)
	if !health.Allow("catalog") {
		t.Fatal("plain status text incorrectly triggered typed HTTP policy cooldown")
	}
}

func TestSourceHealthRateLimitHonorsRetryAfter(t *testing.T) {
	clock := &healthClock{now: time.Unix(1000, 0)}
	health := newSourceHealth(clock.Now)

	health.RecordFailure("catalog", sourceHTTPError{
		status:     429,
		retryAfter: 17 * time.Minute,
	})

	clock.Advance(17*time.Minute - time.Nanosecond)
	if health.Allow("catalog") {
		t.Fatal("Allow() = true before Retry-After elapsed")
	}
	clock.Advance(time.Nanosecond)
	if !health.Allow("catalog") {
		t.Fatal("Allow() = false after Retry-After elapsed")
	}
}

func TestSourceHealthHonorsRetryAfterFromUpstreamHTTPResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	_, err := httpGetBytes(context.Background(), server.Client(), server.URL, "test-agent")
	if err == nil {
		t.Fatal("httpGetBytes() error = nil, want 429")
	}

	clock := &healthClock{now: time.Unix(1000, 0)}
	health := newSourceHealth(clock.Now)
	health.RecordFailure("openlibrary", err)

	clock.Advance(90*time.Second - time.Nanosecond)
	if health.Allow("openlibrary") {
		t.Fatal("Allow() = true before upstream Retry-After elapsed")
	}
	clock.Advance(time.Nanosecond)
	if !health.Allow("openlibrary") {
		t.Fatal("Allow() = false after upstream Retry-After elapsed")
	}
}

func TestSourceHealthTimeoutsUseExponentialCooldown(t *testing.T) {
	clock := &healthClock{now: time.Unix(1000, 0)}
	health := newSourceHealth(clock.Now)

	health.RecordFailure("catalog", context.DeadlineExceeded)
	clock.Advance(healthTimeoutBase - time.Nanosecond)
	if health.Allow("catalog") {
		t.Fatal("Allow() = true before first timeout cooldown elapsed")
	}
	clock.Advance(time.Nanosecond)
	if !health.Allow("catalog") {
		t.Fatal("Allow() = false after first timeout cooldown elapsed")
	}

	health.RecordFailure("catalog", context.DeadlineExceeded)
	clock.Advance(2*healthTimeoutBase - time.Nanosecond)
	if health.Allow("catalog") {
		t.Fatal("Allow() = true before second timeout cooldown elapsed")
	}
	clock.Advance(time.Nanosecond)
	if !health.Allow("catalog") {
		t.Fatal("Allow() = false after second timeout cooldown elapsed")
	}
}

func TestSourceHealthSuccessClosesCircuit(t *testing.T) {
	clock := &healthClock{now: time.Unix(1000, 0)}
	health := newSourceHealth(clock.Now)

	health.RecordFailure("catalog", sourceHTTPError{status: 403})
	health.RecordSuccess("catalog")

	if !health.Allow("catalog") {
		t.Fatal("Allow() = false after successful response")
	}
}

func TestSourceHealthRetryAfterReportsRemainingCooldown(t *testing.T) {
	clock := &healthClock{now: time.Unix(1000, 0)}
	health := newSourceHealth(clock.Now)
	health.RecordFailure("catalog", sourceHTTPError{
		status:     429,
		retryAfter: 17 * time.Second,
	})

	clock.Advance(5 * time.Second)
	retryAfter, blocked := health.RetryAfter("catalog")

	if !blocked {
		t.Fatal("RetryAfter() blocked = false during cooldown")
	}
	if retryAfter != 12*time.Second {
		t.Fatalf("RetryAfter() = %s, want 12s", retryAfter)
	}
}

func TestProviderSkipsSourceDuringHealthCooldown(t *testing.T) {
	clock := &healthClock{now: time.Unix(1000, 0)}
	source := &fakeSource{
		id:  "openlibrary",
		err: sourceHTTPError{status: 403},
	}
	p := NewProviderWithSources([]Source{source})
	p.health = newSourceHealth(clock.Now)
	query := metadata.SearchQuery{Title: "Dune"}

	if _, err := p.Search(context.Background(), query); err == nil {
		t.Fatal("first Search() error = nil, want source availability error")
	}
	if _, err := p.Search(context.Background(), query); err == nil {
		t.Fatal("second Search() error = nil, want cooldown availability error")
	}
	if source.searchCalls != 1 {
		t.Fatalf("search calls during cooldown = %d, want 1", source.searchCalls)
	}

	clock.Advance(time.Hour)
	if _, err := p.Search(context.Background(), query); err == nil {
		t.Fatal("Search() after cooldown error = nil, want source availability error")
	}
	if source.searchCalls != 2 {
		t.Fatalf("search calls after cooldown = %d, want 2", source.searchCalls)
	}
}

func TestProviderCancellationDoesNotOpenSourceCircuit(t *testing.T) {
	source := &fakeSource{
		id:  "openlibrary",
		err: context.Canceled,
	}
	p := NewProviderWithSources([]Source{source})
	query := metadata.SearchQuery{Title: "Dune"}

	for i := 0; i < 2; i++ {
		if _, err := p.Search(context.Background(), query); !errors.Is(err, context.Canceled) {
			t.Fatalf("Search() %d error = %v, want context.Canceled", i+1, err)
		}
	}
	if source.searchCalls != 2 {
		t.Fatalf("search calls after cancellation = %d, want 2", source.searchCalls)
	}
}
