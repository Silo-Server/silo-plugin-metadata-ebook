package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPGetBytesLimitsBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(make([]byte, maxResponseBytes+1)); err != nil {
			t.Errorf("server write: %v", err)
		}
	}))
	defer server.Close()

	_, err := httpGetBytes(context.Background(), server.Client(), server.URL, "")
	if err == nil {
		t.Fatal("httpGetBytes() error = nil, want body limit error")
	}
}

func TestHTTPGetBytesSetsUserAgent(t *testing.T) {
	var gotUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.UserAgent()
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Errorf("server write: %v", err)
		}
	}))
	defer server.Close()

	got, err := httpGetBytes(context.Background(), server.Client(), server.URL, "test-agent")
	if err != nil {
		t.Fatalf("httpGetBytes() error = %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("httpGetBytes() = %q, want ok", string(got))
	}
	if gotUserAgent != "test-agent" {
		t.Fatalf("User-Agent = %q, want test-agent", gotUserAgent)
	}
}

func TestHTTPGetBytesErrorsForNon2xxStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	_, err := httpGetBytes(context.Background(), server.Client(), server.URL, "")
	if err == nil {
		t.Fatal("httpGetBytes() error = nil, want status error")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("httpGetBytes() error = %v, want status error", err)
	}
}

func TestHTTPGetBytesReturnsTypedStatusWithDeltaRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := httpGetBytes(context.Background(), server.Client(), server.URL, "")

	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("httpGetBytes() error = %T %v, want *HTTPStatusError", err, err)
	}
	if statusErr.HTTPStatusCode() != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatusCode() = %d, want 429", statusErr.HTTPStatusCode())
	}
	if statusErr.RetryAfter() != 2*time.Minute {
		t.Fatalf("RetryAfter() = %v, want 2m", statusErr.RetryAfter())
	}
}

func TestHTTPStatusErrorParsesHTTPDateRetryAfter(t *testing.T) {
	base := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	header := make(http.Header)
	header.Set("Retry-After", base.Add(17*time.Minute).Format(http.TimeFormat))

	err := newHTTPStatusError(http.MethodGet, "https://example.test", http.StatusTooManyRequests, header, base)

	if err.RetryAfter() != 17*time.Minute {
		t.Fatalf("RetryAfter() = %v, want 17m", err.RetryAfter())
	}
}

func TestHTTPDoBytesReturnsTypedStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, status, err := httpDoBytes(context.Background(), server.Client(), req)

	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("httpDoBytes() error = %T %v, want *HTTPStatusError", err, err)
	}
	if status != http.StatusForbidden || statusErr.HTTPStatusCode() != http.StatusForbidden {
		t.Fatalf("statuses = %d and %d, want 403", status, statusErr.HTTPStatusCode())
	}
}

func TestHTTPGetBytesWrapsRequestErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := httpGetBytes(ctx, http.DefaultClient, "https://example.invalid", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("httpGetBytes() error = %v, want context.Canceled", err)
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func (brokenBody) Close() error {
	return nil
}

type brokenBodyTransport struct{}

func (brokenBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       brokenBody{},
	}, nil
}

func TestHTTPGetBytesWrapsBodyReadErrors(t *testing.T) {
	client := &http.Client{Transport: brokenBodyTransport{}}

	_, err := httpGetBytes(context.Background(), client, "https://example.test/book", "")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("httpGetBytes() error = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestRedactRawURLMasksQueryValuesAndSourceIDs(t *testing.T) {
	got := redactRawURL("https://example.test/search?q=Project+Hail+Mary&key=secret")
	for _, leaked := range []string{"Project", "Hail", "Mary", "secret"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redactRawURL() = %q, leaked %q", got, leaked)
		}
	}
	if !strings.Contains(got, "q=%3Credacted%3E") || !strings.Contains(got, "key=%3Credacted%3E") {
		t.Fatalf("redactRawURL() = %q, want redacted query values", got)
	}

	for _, raw := range []string{
		"https://example.test/dp/B08G9PRS1K",
		"https://example.test/md5/a1b2c3d4e5f67890abcdef1234567890",
	} {
		got = redactRawURL(raw)
		if strings.Contains(got, "B08G9PRS1K") || strings.Contains(got, "a1b2c3d4e5f67890abcdef1234567890") {
			t.Fatalf("redactRawURL(%q) = %q, leaked source ID", raw, got)
		}
		if !strings.Contains(got, "/redacted") {
			t.Fatalf("redactRawURL(%q) = %q, want redacted path segment", raw, got)
		}
	}
}

func TestRateLimiterInvalidRPMDoesNotGrantRequest(t *testing.T) {
	for _, rpm := range []float64{0, -1} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		err := waitForLimiter(ctx, newLimiter(rpm))
		cancel()
		if err == nil {
			t.Fatalf("waitForLimiter(rpm=%v) error = nil, want invalid limiter error", rpm)
		}
	}
}

func TestRateLimiterPositiveRPMAllowsInitialRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := waitForLimiter(ctx, newLimiter(60)); err != nil {
		t.Fatalf("waitForLimiter() error = %v", err)
	}
}

func TestRateLimiterWaitsForShortDelaysAndAdmits(t *testing.T) {
	// The next token being a fraction of a second away is the steady-state of
	// a saturated 60rpm source. Skipping there starved the backfill drain
	// (every attempt deferred); a bounded wait converts it into throughput.
	limiter := newLimiter(300)
	if err := waitForLimiter(context.Background(), limiter); err != nil {
		t.Fatalf("initial waitForLimiter() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	started := time.Now()
	if err := waitForLimiter(ctx, limiter); err != nil {
		t.Fatalf("waitForLimiter() error = %v, want admission after a short wait", err)
	}
	elapsed := time.Since(started)
	if elapsed < 100*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("waitForLimiter() took %s, want ~200ms bounded wait", elapsed)
	}
}

func TestRateLimiterSkipsImmediatelyWhenDelayExceedsCap(t *testing.T) {
	// rpm=1 → after the initial token the next is a minute out: far past the
	// wait cap. A worker must skip instantly (with the real retry hint), so a
	// pile-up on one saturated source can never stall a source chain.
	limiter := newLimiter(1)
	if err := waitForLimiter(context.Background(), limiter); err != nil {
		t.Fatalf("initial waitForLimiter() error = %v", err)
	}

	started := time.Now()
	err := waitForLimiter(context.Background(), limiter)

	var admissionErr *sourceAdmissionError
	if !errors.As(err, &admissionErr) {
		t.Fatalf("waitForLimiter() error = %T %v, want sourceAdmissionError", err, err)
	}
	if elapsed := time.Since(started); elapsed >= 50*time.Millisecond {
		t.Fatalf("waitForLimiter() took %s, want immediate skip", elapsed)
	}
	if got := admissionErr.RetryAfter(); got < 30*time.Second {
		t.Fatalf("sourceAdmissionError.RetryAfter() = %s, want the real token delay", got)
	}
}

func TestRateLimiterSkipsWhenParentDeadlineCannotCoverWait(t *testing.T) {
	// A short-deadline caller (interactive search) must not burn its budget
	// sleeping for a token it can't use.
	limiter := newLimiter(300)
	if err := waitForLimiter(context.Background(), limiter); err != nil {
		t.Fatalf("initial waitForLimiter() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := waitForLimiter(ctx, limiter)

	var admissionErr *sourceAdmissionError
	if !errors.As(err, &admissionErr) {
		t.Fatalf("waitForLimiter() error = %T %v, want sourceAdmissionError", err, err)
	}
	if elapsed := time.Since(started); elapsed >= 40*time.Millisecond {
		t.Fatalf("waitForLimiter() took %s, want immediate skip under a tight deadline", elapsed)
	}
	if ctx.Err() != nil {
		t.Fatalf("waitForLimiter() consumed parent context: %v", ctx.Err())
	}
}

func TestAllRateLimitRetryAfterAcceptsUpstream429(t *testing.T) {
	err := &HTTPStatusError{
		method:     "GET",
		requestURL: "https://example.test/books",
		statusCode: http.StatusTooManyRequests,
		retryAfter: 9 * time.Second,
	}

	retryAfter, ok := allRateLimitRetryAfter([]error{err})

	if !ok || retryAfter != 9*time.Second {
		t.Fatalf("allRateLimitRetryAfter() = %s, %v; want 9s, true", retryAfter, ok)
	}
}
