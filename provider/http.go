package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 4 * 1024 * 1024

type HTTPStatusError struct {
	method     string
	requestURL string
	statusCode int
	retryAfter time.Duration
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d", e.method, e.requestURL, e.statusCode)
}

func (e *HTTPStatusError) HTTPStatusCode() int {
	return e.statusCode
}

func (e *HTTPStatusError) RetryAfter() time.Duration {
	return e.retryAfter
}

func newHTTPStatusError(method, requestURL string, statusCode int, header http.Header, now time.Time) *HTTPStatusError {
	return &HTTPStatusError{
		method:     method,
		requestURL: requestURL,
		statusCode: statusCode,
		retryAfter: parseRetryAfter(header.Get("Retry-After"), now),
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	if retryAfter := retryAt.Sub(now); retryAfter > 0 {
		return retryAfter
	}
	return 0
}

func httpGetBytes(ctx context.Context, client *http.Client, url string, userAgent string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get %s: request: %w", redactRawURL(url), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newHTTPStatusError(http.MethodGet, redactRawURL(url), resp.StatusCode, resp.Header, time.Now())
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("http get %s: read body: %w", redactRawURL(url), err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("http get %s: response body exceeds %d bytes", redactRawURL(url), maxResponseBytes)
	}

	return body, nil
}

func redactRawURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return redactURL(parsed)
}
