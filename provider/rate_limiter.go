package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"golang.org/x/time/rate"
)

var errSourceAdmissionUnavailable = errors.New("rate limit token not immediately available")
var errSourceHealthCooldown = errors.New("source health cooldown is active")

// sourceAdmissionError means a single source could not obtain a rate-limit
// token promptly. It is not a source health failure.
type sourceAdmissionError struct {
	cause      error
	retryAfter time.Duration
}

func (e *sourceAdmissionError) Error() string {
	return fmt.Sprintf("source rate-limit admission skipped: %v", e.cause)
}

func (e *sourceAdmissionError) Unwrap() error {
	return e.cause
}

func (e *sourceAdmissionError) RetryAfter() time.Duration {
	return e.retryAfter
}

// newLimiter creates a token-bucket rate limiter allowing rpm requests per minute.
// A burst of 1 is used so requests are spaced evenly rather than batched.
func newLimiter(rpm float64) *rate.Limiter {
	if rpm <= 0 || math.IsNaN(rpm) || math.IsInf(rpm, 0) {
		return rate.NewLimiter(0, 0)
	}
	return rate.NewLimiter(rate.Limit(rpm/60.0), 1)
}

// setLimiterRPM retunes an existing limiter in place. rate.Limiter is
// goroutine-safe, so mutating it avoids the data race of swapping the
// client's limiter pointer while worker goroutines are reading it.
func setLimiterRPM(l *rate.Limiter, rpm float64) {
	if rpm <= 0 || math.IsNaN(rpm) || math.IsInf(rpm, 0) {
		l.SetLimit(0)
		l.SetBurst(0)
		return
	}
	l.SetLimit(rate.Limit(rpm / 60.0))
	l.SetBurst(1)
}

// admissionWaitCap bounds how long admission may sleep for the next token.
// A saturated 60rpm source's next token is under a second away, so waits at
// or below the cap admit after a short sleep — skipping there starved the
// backfill drain, deferring every claim while tokens sat idle. Longer waits
// (and waits a caller's deadline cannot cover) still skip immediately, so a
// pile of workers can never stall a source chain behind one dead source.
const admissionWaitCap = 2 * time.Second

// waitForLimiter admits after at most admissionWaitCap, or skips this source
// with the real token delay as the retry hint.
func waitForLimiter(ctx context.Context, l *rate.Limiter) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	now := time.Now()
	reservation := l.ReserveN(now, 1)
	if !reservation.OK() {
		return &sourceAdmissionError{
			cause:      errSourceAdmissionUnavailable,
			retryAfter: time.Minute,
		}
	}
	retryAfter := reservation.DelayFrom(now)
	if retryAfter <= 0 {
		return nil
	}
	if retryAfter <= admissionWaitCap && deadlineCovers(ctx, retryAfter) {
		timer := time.NewTimer(retryAfter)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			reservation.CancelAt(time.Now())
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	reservation.CancelAt(now)
	return &sourceAdmissionError{
		cause:      errSourceAdmissionUnavailable,
		retryAfter: retryAfter,
	}
}

// deadlineCovers reports whether ctx's deadline (if any) leaves room to sleep
// for wait and still do useful work afterwards.
func deadlineCovers(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > wait
}
