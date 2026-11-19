package provider

import (
	"context"
	"fmt"
	"math"
	"net/http"

	"golang.org/x/time/rate"
)

// newLimiter creates a token-bucket rate limiter allowing rpm requests per minute.
// A burst of 1 is used so requests are spaced evenly rather than batched.
func newLimiter(rpm float64) *rate.Limiter {
	if rpm <= 0 || math.IsNaN(rpm) || math.IsInf(rpm, 0) {
		return rate.NewLimiter(0, 0)
	}
	return rate.NewLimiter(rate.Limit(rpm/60.0), 1)
}

// waitForLimiter blocks until the limiter permits a request or ctx is cancelled.
func waitForLimiter(ctx context.Context, l *rate.Limiter) error {
	return l.Wait(ctx)
}

// rateLimitedTransport holds every request, a followed redirect included, until
// the limiter permits it. A request whose wait would outlast its context fails
// without being sent.
type rateLimitedTransport struct {
	limiter *rate.Limiter
}

func (t rateLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := waitForLimiter(req.Context(), t.limiter); err != nil {
		return nil, fmt.Errorf("rate limit: %w", err)
	}
	return http.DefaultTransport.RoundTrip(req)
}
