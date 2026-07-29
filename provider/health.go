package provider

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

const (
	healthPolicyCooldown   = time.Hour
	healthRateLimitDefault = 5 * time.Minute
	healthTimeoutBase      = 30 * time.Second
	healthCooldownMax      = time.Hour
)

type sourceHealthState struct {
	failures int
	until    time.Time
}

type SourceHealth struct {
	mu     sync.Mutex
	now    func() time.Time
	states map[string]sourceHealthState
}

func NewSourceHealth() *SourceHealth {
	return newSourceHealth(time.Now)
}

func newSourceHealth(now func() time.Time) *SourceHealth {
	return &SourceHealth{
		now:    now,
		states: make(map[string]sourceHealthState),
	}
}

func (h *SourceHealth) Allow(sourceID string) bool {
	_, blocked := h.RetryAfter(sourceID)
	return !blocked
}

func (h *SourceHealth) RetryAfter(sourceID string) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	state, ok := h.states[sourceID]
	if !ok {
		return 0, false
	}
	retryAfter := state.until.Sub(h.currentTime())
	if retryAfter <= 0 {
		return 0, false
	}
	return retryAfter, true
}

func (h *SourceHealth) RecordSuccess(sourceID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.states, sourceID)
}

func (h *SourceHealth) RecordFailure(sourceID string, err error) {
	if err == nil {
		h.RecordSuccess(sourceID)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.states == nil {
		h.states = make(map[string]sourceHealthState)
	}
	state := h.states[sourceID]
	state.failures++
	cooldown := failureCooldown(err, state.failures)
	state.until = h.currentTime().Add(cooldown)
	h.states[sourceID] = state
}

func (h *SourceHealth) currentTime() time.Time {
	if h.now == nil {
		return time.Now()
	}
	return h.now()
}

func failureCooldown(err error, failures int) time.Duration {
	switch statusCode(err) {
	case 403, 405:
		return healthPolicyCooldown
	case 429:
		if retryAfter := retryAfterDuration(err); retryAfter > 0 {
			return retryAfter
		}
		return healthRateLimitDefault
	}

	if isTimeout(err) {
		return exponentialCooldown(failures)
	}
	return exponentialCooldown(failures)
}

func exponentialCooldown(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	cooldown := healthTimeoutBase
	for i := 1; i < failures && cooldown < healthCooldownMax; i++ {
		cooldown *= 2
	}
	if cooldown > healthCooldownMax {
		return healthCooldownMax
	}
	return cooldown
}

func statusCode(err error) int {
	type statusCoder interface {
		HTTPStatusCode() int
	}
	var withStatus statusCoder
	if errors.As(err, &withStatus) {
		return withStatus.HTTPStatusCode()
	}

	return 0
}

func retryAfterDuration(err error) time.Duration {
	type retryAfterer interface {
		RetryAfter() time.Duration
	}
	var withRetryAfter retryAfterer
	if errors.As(err, &withRetryAfter) {
		return withRetryAfter.RetryAfter()
	}
	return 0
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
