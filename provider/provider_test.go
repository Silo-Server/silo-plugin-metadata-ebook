package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

type fakeSource struct {
	id          string
	search      []metadata.Match
	searchFn    func(context.Context, metadata.SearchQuery) ([]metadata.Match, error)
	fetch       *metadata.Match
	fetchFn     func(context.Context, string) (*metadata.Match, error)
	fetchID     string
	err         error
	fetched     []string
	searchCalls int
}

func (s *fakeSource) ID() string {
	return s.id
}

func (s *fakeSource) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	s.searchCalls++
	if s.searchFn != nil {
		return s.searchFn(ctx, q)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.search, nil
}

func (s *fakeSource) Fetch(ctx context.Context, id string) (*metadata.Match, error) {
	s.fetched = append(s.fetched, id)
	if s.fetchFn != nil {
		return s.fetchFn(ctx, id)
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.fetchID != "" && id != s.fetchID {
		return nil, nil
	}
	return s.fetch, nil
}

func requireSourcesUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want source availability error")
	}
	if !strings.Contains(err.Error(), "sources unavailable") {
		t.Fatalf("error = %v, want source availability error", err)
	}
	if !errors.Is(err, ErrSourcesUnavailable) {
		t.Fatalf("error = %v, want errors.Is(ErrSourcesUnavailable)", err)
	}
}

func TestProviderSearchReturnsAvailabilityErrorWhenEverySourceFails(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "catalog-one", err: errors.New("one failed")},
		&fakeSource{id: "catalog-two", err: errors.New("two failed")},
	})

	matches, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Dune"})

	if matches != nil {
		t.Fatalf("Search() matches = %#v, want nil", matches)
	}
	requireSourcesUnavailable(t, err)
}

func TestProviderSearchClassifiesAllAdmissionFailuresAsRateLimited(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "catalog-one", err: &sourceAdmissionError{cause: errSourceAdmissionUnavailable, retryAfter: time.Second}},
		&fakeSource{id: "catalog-two", err: &sourceAdmissionError{cause: errSourceAdmissionUnavailable, retryAfter: 2 * time.Second}},
	})

	_, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Dune"})

	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("Search() error = %T %v, want *RateLimitedError", err, err)
	}
	if rateLimited.RetryAfter() != time.Second {
		t.Fatalf("RetryAfter() = %s, want earliest source admission in 1s", rateLimited.RetryAfter())
	}
	if !errors.Is(err, ErrSourcesUnavailable) {
		t.Fatalf("Search() error = %v, want errors.Is(ErrSourcesUnavailable)", err)
	}
}

func TestProviderSourceAdmissionIsSharedAcrossRequests(t *testing.T) {
	p := NewProviderWithSources([]Source{&fakeSource{id: "catalog-one"}})
	releases := make([]func(), 0, sourceCallConcurrency)
	for range sourceCallConcurrency {
		release, err := p.acquireSource("catalog-one")
		if err != nil {
			t.Fatalf("acquire within source limit: %v", err)
		}
		releases = append(releases, release)
	}

	if _, err := p.acquireSource("catalog-one"); !errors.Is(err, errSourceAdmissionUnavailable) {
		t.Fatalf("acquire above source limit = %v, want admission unavailable", err)
	}

	releases[0]()
	release, err := p.acquireSource("catalog-one")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release()
	for _, release := range releases[1:] {
		release()
	}
}

func TestProviderSearchDoesNotMisclassifyMixedFailuresAsRateLimited(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "catalog-one", err: &sourceAdmissionError{cause: errSourceAdmissionUnavailable, retryAfter: time.Second}},
		&fakeSource{id: "catalog-two", err: errors.New("upstream failed")},
	})

	_, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Dune"})

	var rateLimited *RateLimitedError
	if errors.As(err, &rateLimited) {
		t.Fatalf("Search() error = %T %v, do not want rate-limited classification for mixed failures", err, err)
	}
	requireSourcesUnavailable(t, err)
}

func TestProviderSearchClassifiesUpstreamRateLimits(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "catalog-one", err: &HTTPStatusError{
			method:     "GET",
			requestURL: "https://example.test/books",
			statusCode: 429,
			retryAfter: 17 * time.Second,
		}},
	})

	_, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Dune"})

	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("Search() error = %T %v, want *RateLimitedError", err, err)
	}
	if rateLimited.RetryAfter() != 17*time.Second {
		t.Fatalf("RetryAfter() = %s, want upstream Retry-After 17s", rateLimited.RetryAfter())
	}
}

func TestProviderSearchKeepsPartialFailuresIsolatedAfterHealthyNoMatch(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "healthy"},
		&fakeSource{id: "failing", err: errors.New("failed")},
	})

	matches, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Dune"})

	if err != nil {
		t.Fatalf("Search() error = %v, want nil after a healthy no-match", err)
	}
	if len(matches) != 0 {
		t.Fatalf("Search() matches = %#v, want none", matches)
	}
}

func TestProviderSearchCooldownSkipsDoNotCountAsSuccessfulAttempts(t *testing.T) {
	source := &fakeSource{id: "catalog"}
	p := NewProviderWithSources([]Source{source})
	p.health.RecordFailure(source.ID(), errors.New("temporarily unavailable"))

	matches, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Dune"})

	if matches != nil {
		t.Fatalf("Search() matches = %#v, want nil", matches)
	}
	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("Search() error = %T %v, want retryable cooldown", err, err)
	}
	if rateLimited.RetryAfter() <= 0 {
		t.Fatalf("RetryAfter() = %s, want positive cooldown", rateLimited.RetryAfter())
	}
	if source.searchCalls != 0 {
		t.Fatalf("source search calls = %d, want cooldown skip", source.searchCalls)
	}
}

func TestProviderSearchCredentiallessNoOpsDoNotMaskSourceFailure(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "openlibrary", err: errors.New("openlibrary failed")},
		NewGoogleBooksClient("", "test-agent"),
		NewISBNdbClient("", "test-agent"),
		NewHardcoverClient("", "test-agent"),
	})

	matches, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Dune"})

	if matches != nil {
		t.Fatalf("Search() matches = %#v, want nil", matches)
	}
	requireSourcesUnavailable(t, err)
}

func TestProviderFetchReturnsAvailabilityErrorWhenEveryEligibleSourceFails(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "openlibrary", err: errors.New("openlibrary failed")},
		&fakeSource{id: "googlebooks", err: errors.New("googlebooks failed")},
	})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-593-13520-4"},
	})

	if match != nil {
		t.Fatalf("Fetch() match = %#v, want nil", match)
	}
	requireSourcesUnavailable(t, err)
}

func TestProviderFetchKeepsPartialFailuresIsolatedAfterHealthyNoMatch(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "openlibrary"},
		&fakeSource{id: "googlebooks", err: errors.New("googlebooks failed")},
	})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-593-13520-4"},
	})

	if err != nil {
		t.Fatalf("Fetch() error = %v, want nil after a healthy no-match", err)
	}
	if match != nil {
		t.Fatalf("Fetch() match = %#v, want nil", match)
	}
}

func TestProviderFetchCooldownSkipDoesNotCountAsSuccessfulAttempt(t *testing.T) {
	source := &fakeSource{id: "openlibrary"}
	p := NewProviderWithSources([]Source{source})
	p.health.RecordFailure(source.ID(), errors.New("temporarily unavailable"))

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-593-13520-4"},
	})

	if match != nil {
		t.Fatalf("Fetch() match = %#v, want nil", match)
	}
	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("Fetch() error = %T %v, want retryable cooldown", err, err)
	}
	if rateLimited.RetryAfter() <= 0 {
		t.Fatalf("RetryAfter() = %s, want positive cooldown", rateLimited.RetryAfter())
	}
	if len(source.fetched) != 0 {
		t.Fatalf("source fetch calls = %d, want cooldown skip", len(source.fetched))
	}
}

func TestProviderSearchPreservesParentCancellationWithoutPenalizingHealth(t *testing.T) {
	source := &fakeSource{
		id: "catalog",
		searchFn: func(ctx context.Context, _ metadata.SearchQuery) ([]metadata.Match, error) {
			return nil, ctx.Err()
		},
	}
	p := NewProviderWithSources([]Source{source})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Search(ctx, metadata.SearchQuery{Title: "Dune"})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error = %v, want context.Canceled", err)
	}
	if !p.health.Allow(source.ID()) {
		t.Fatal("parent cancellation opened source health circuit")
	}
}

func TestProviderParentDeadlineDoesNotPenalizeSourceHealth(t *testing.T) {
	source := &fakeSource{
		id: "catalog",
		searchFn: func(ctx context.Context, _ metadata.SearchQuery) ([]metadata.Match, error) {
			return nil, ctx.Err()
		},
	}
	p := NewProviderWithSources([]Source{source})
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	_, err := p.Search(ctx, metadata.SearchQuery{Title: "Dune"})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Search() error = %v, want context.DeadlineExceeded", err)
	}
	if !p.health.Allow(source.ID()) {
		t.Fatal("provider context deadline opened source health circuit")
	}
}

func TestProviderDeadlineIsPreservedWithoutPenalizingSourceHealth(t *testing.T) {
	source := &fakeSource{
		id: "catalog",
		searchFn: func(ctx context.Context, _ metadata.SearchQuery) ([]metadata.Match, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	p := NewProviderWithSources([]Source{source})
	p.timeout = time.Millisecond

	_, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Dune"})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Search() error = %v, want provider context.DeadlineExceeded", err)
	}
	if !p.health.Allow(source.ID()) {
		t.Fatal("provider deadline opened source health circuit")
	}
}

func TestProviderSearchPreservesHealthyMatchesAtInternalDeadline(t *testing.T) {
	healthy := &fakeSource{
		id: "catalog-fast",
		search: []metadata.Match{{
			Provider:   "catalog-fast",
			ProviderID: "book-1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
		}},
	}
	slow := &fakeSource{
		id: "catalog-slow",
		searchFn: func(ctx context.Context, _ metadata.SearchQuery) ([]metadata.Match, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	p := NewProviderWithSources([]Source{healthy, slow})
	p.timeout = time.Millisecond

	matches, err := p.Search(context.Background(), metadata.SearchQuery{
		Title:   "Dune",
		Authors: []string{"Frank Herbert"},
	})

	if err != nil {
		t.Fatalf("Search() error = %v, want healthy partial result", err)
	}
	if len(matches) != 1 || matches[0].ProviderID != "book-1" {
		t.Fatalf("Search() = %#v, want healthy partial result", matches)
	}
}

func TestProviderFetchPreservesParentCancellationWithoutPenalizingHealth(t *testing.T) {
	source := &fakeSource{
		id: "openlibrary",
		fetchFn: func(ctx context.Context, _ string) (*metadata.Match, error) {
			return nil, ctx.Err()
		},
	}
	p := NewProviderWithSources([]Source{source})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Fetch(ctx, metadata.SearchQuery{
		ProviderIDs: map[string]string{"openlibrary": "OL1M"},
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch() error = %v, want context.Canceled", err)
	}
	if !p.health.Allow(source.ID()) {
		t.Fatal("parent cancellation opened source health circuit")
	}
}

func TestProviderSourceOwnedTimeoutPenalizesHealth(t *testing.T) {
	source := &fakeSource{id: "catalog", err: context.DeadlineExceeded}
	p := NewProviderWithSources([]Source{source})

	_, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Dune"})

	requireSourcesUnavailable(t, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Search() error = %v, want wrapped source timeout", err)
	}
	if p.health.Allow(source.ID()) {
		t.Fatal("source-owned timeout did not open source health circuit")
	}
}

func TestProviderSearchSwallowsPerSourceErrors(t *testing.T) {
	goodMatch := metadata.Match{
		Provider:   "openlibrary",
		ProviderID: "OL1",
		Title:      "Good Book",
	}
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "openlibrary", search: []metadata.Match{goodMatch}},
		&fakeSource{id: "badsource", err: errors.New("source failed")},
	})

	matches, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Good Book"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("Search() returned %d matches, want 1", len(matches))
	}
	if matches[0].ProviderID != "OL1" {
		t.Fatalf("Search()[0].ProviderID = %q, want OL1", matches[0].ProviderID)
	}
}

func TestProviderSearchAdmissionSkipPreservesHealthyResults(t *testing.T) {
	// rpm=1: the next token is a minute out, far beyond the bounded
	// admission wait, so the source skips instantly. (Short delays now admit
	// after a bounded wait instead — see TestRateLimiterWaitsForShortDelaysAndAdmits.)
	limiter := newLimiter(1)
	if err := waitForLimiter(context.Background(), limiter); err != nil {
		t.Fatalf("prime limiter: %v", err)
	}
	limited := &fakeSource{
		id: "openlibrary",
		searchFn: func(ctx context.Context, _ metadata.SearchQuery) ([]metadata.Match, error) {
			return nil, waitForLimiter(ctx, limiter)
		},
	}
	healthy := &fakeSource{
		id: "googlebooks",
		search: []metadata.Match{{
			Provider:   "googlebooks",
			ProviderID: "GB1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
		}},
	}
	p := NewProviderWithSources([]Source{limited, healthy})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	started := time.Now()
	matches, err := p.Search(ctx, metadata.SearchQuery{Title: "Dune"})

	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 50*time.Millisecond {
		t.Fatalf("Search() took %s, want limiter skip not parent deadline", elapsed)
	}
	if len(matches) != 1 || matches[0].ProviderID != "GB1" {
		t.Fatalf("Search() = %#v, want healthy provider result", matches)
	}
	if !p.health.Allow(limited.ID()) {
		t.Fatal("source-local admission skip opened health circuit")
	}
}

func TestProviderSearchISBNAdmissionSkipReachesLaterProvider(t *testing.T) {
	const isbn = "9780441172719"
	// rpm=1 keeps the next token beyond the bounded admission wait so the
	// skip path (not the new short-delay wait) is what's under test.
	limiter := newLimiter(1)
	if err := waitForLimiter(context.Background(), limiter); err != nil {
		t.Fatalf("prime limiter: %v", err)
	}
	limited := &fakeSource{
		id: "openlibrary",
		fetchFn: func(ctx context.Context, _ string) (*metadata.Match, error) {
			return nil, waitForLimiter(ctx, limiter)
		},
	}
	healthy := &fakeSource{
		id:      "googlebooks",
		fetchID: isbn,
		fetch: &metadata.Match{
			Provider:   "googlebooks",
			ProviderID: "GB1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
			ISBN:       isbn,
		},
	}
	p := NewProviderWithSources([]Source{limited, healthy})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	matches, err := p.Search(ctx, metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": isbn},
	})

	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(matches) != 1 || matches[0].ProviderID != "GB1" {
		t.Fatalf("Search() = %#v, want later ISBN provider result", matches)
	}
	if len(healthy.fetched) != 1 {
		t.Fatalf("later provider fetches = %#v, want one", healthy.fetched)
	}
	if !p.health.Allow(limited.ID()) {
		t.Fatal("source-local admission skip opened health circuit")
	}
}

func TestProviderFetchRoutesCapabilityID(t *testing.T) {
	fetched := &metadata.Match{
		Provider:   "openlibrary",
		ProviderID: "OL1",
		Title:      "Fetched Book",
	}
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "openlibrary", fetch: fetched, fetchID: "OL1"},
	})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{metadata.CapabilityID: "openlibrary:OL1"},
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if match == nil {
		t.Fatal("Fetch() returned nil, want match")
	}
	if match.ProviderID != "OL1" {
		t.Fatalf("Fetch().ProviderID = %q, want OL1", match.ProviderID)
	}
}

func TestProviderFetchRoutesSourceSpecificID(t *testing.T) {
	fetched := &metadata.Match{
		Provider:   "googlebooks",
		ProviderID: "GB1",
		Title:      "Fetched Book",
	}
	p := NewProviderWithSources([]Source{
		&fakeSource{id: "googlebooks", fetch: fetched, fetchID: "GB1"},
	})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"googlebooks": "GB1"},
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if match == nil {
		t.Fatal("Fetch() returned nil, want match")
	}
	if match.ProviderID != "GB1" {
		t.Fatalf("Fetch().ProviderID = %q, want GB1", match.ProviderID)
	}
}

func TestProviderFetchWaitsForSelectedSourceAdmission(t *testing.T) {
	var attempts int
	source := &fakeSource{
		id: "openlibrary",
		fetchFn: func(context.Context, string) (*metadata.Match, error) {
			attempts++
			if attempts == 1 {
				return nil, &sourceAdmissionError{
					cause:      errSourceAdmissionUnavailable,
					retryAfter: time.Millisecond,
				}
			}
			return &metadata.Match{
				Provider:    "openlibrary",
				ProviderID:  "OL1M",
				Title:       "Dune",
				Description: "full detail",
			}, nil
		},
	}
	p := NewProviderWithSources([]Source{source})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"openlibrary": "OL1M"},
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if match == nil || match.Description != "full detail" {
		t.Fatalf("Fetch() = %#v, want full detail", match)
	}
	if attempts != 2 {
		t.Fatalf("fetch attempts = %d, want admission retry", attempts)
	}
}

func TestProviderFetchDoesNotWaitPastDeadlineForAdmission(t *testing.T) {
	source := &fakeSource{
		id: "openlibrary",
		fetchFn: func(context.Context, string) (*metadata.Match, error) {
			return nil, &sourceAdmissionError{
				cause:      errSourceAdmissionUnavailable,
				retryAfter: time.Minute,
			}
		},
	}
	p := NewProviderWithSources([]Source{source})
	p.timeout = 10 * time.Millisecond

	started := time.Now()
	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"openlibrary": "OL1M"},
	})
	if match != nil {
		t.Fatalf("Fetch() = %#v, want no match", match)
	}
	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("Fetch() error = %T %v, want RateLimitedError", err, err)
	}
	if len(source.fetched) != 1 {
		t.Fatalf("source fetches = %#v, want one attempt", source.fetched)
	}
	if elapsed := time.Since(started); elapsed >= 10*time.Millisecond {
		t.Fatalf("Fetch() took %s, want immediate typed deferral", elapsed)
	}
}

func TestProviderFetchDoesNotWaitForUpstreamRateLimit(t *testing.T) {
	source := &fakeSource{
		id: "openlibrary",
		fetchFn: func(context.Context, string) (*metadata.Match, error) {
			return nil, &HTTPStatusError{
				method:     "GET",
				requestURL: "https://example.test/books/OL1M",
				statusCode: 429,
				retryAfter: time.Second,
			}
		},
	}
	p := NewProviderWithSources([]Source{source})

	started := time.Now()
	_, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"openlibrary": "OL1M"},
	})

	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("Fetch() error = %T %v, want RateLimitedError", err, err)
	}
	if elapsed := time.Since(started); elapsed >= 50*time.Millisecond {
		t.Fatalf("Fetch() took %s, want immediate upstream deferral", elapsed)
	}
}

func TestProviderFetchPrefersSourceSpecificID(t *testing.T) {
	openLibrary := &fakeSource{
		id:      "openlibrary",
		fetch:   &metadata.Match{Provider: "openlibrary", ProviderID: "OL1"},
		fetchID: "OL1",
	}
	googleBooks := &fakeSource{
		id:      "googlebooks",
		fetch:   &metadata.Match{Provider: "googlebooks", ProviderID: "GB1"},
		fetchID: "GB1",
	}
	p := NewProviderWithSources([]Source{openLibrary, googleBooks})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{
			"googlebooks":         "GB1",
			metadata.CapabilityID: "openlibrary:OL1",
			"isbn":                "978-0-593-13520-4",
		},
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if match == nil || match.Provider != "googlebooks" {
		t.Fatalf("Fetch() = %#v, want googlebooks source-specific match", match)
	}
	if len(openLibrary.fetched) != 0 {
		t.Fatalf("openlibrary fetched = %#v, want not called", openLibrary.fetched)
	}
}

func TestProviderFetchISBNFallbackContinuesAfterNilAndError(t *testing.T) {
	openLibrary := &fakeSource{id: "openlibrary", fetchID: "9780593135204"}
	googleBooks := &fakeSource{id: "googlebooks", fetchID: "9780593135204", err: errors.New("temporary")}
	isbndb := &fakeSource{
		id:      "isbndb",
		fetchID: "9780593135204",
		fetch:   &metadata.Match{Provider: "isbndb", ProviderID: "9780593135204"},
	}
	p := NewProviderWithSources([]Source{openLibrary, googleBooks, isbndb})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-593-13520-4"},
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if match == nil || match.Provider != "isbndb" {
		t.Fatalf("Fetch() = %#v, want isbndb fallback match", match)
	}
}

func TestProviderFetchISBNContinuesPastIncompleteMatch(t *testing.T) {
	isbn := "9780593135204"
	openLibrary := &fakeSource{
		id:      "openlibrary",
		fetchID: isbn,
		fetch: &metadata.Match{
			Provider:   "openlibrary",
			ProviderID: isbn,
			Title:      "The Glass Hotel",
			ISBN:       isbn,
		},
	}
	googleBooks := &fakeSource{
		id:      "googlebooks",
		fetchID: isbn,
		fetch: &metadata.Match{
			Provider:   "googlebooks",
			ProviderID: "GB1",
			Title:      "The Glass Hotel",
			Authors:    []string{"Emily St. John Mandel"},
			ISBN:       isbn,
		},
	}
	p := NewProviderWithSources([]Source{openLibrary, googleBooks})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": isbn},
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if match == nil || match.Provider != "googlebooks" {
		t.Fatalf("Fetch() = %#v, want complete googlebooks match", match)
	}
	if len(openLibrary.fetched) != 1 || len(googleBooks.fetched) != 1 {
		t.Fatalf("fetches = openlibrary %#v, googlebooks %#v; want one each", openLibrary.fetched, googleBooks.fetched)
	}
}

func TestProviderFetchISBNRecognizesEquivalentISBN10(t *testing.T) {
	source := &fakeSource{
		id:      "openlibrary",
		fetchID: "9780441172719",
		fetch: &metadata.Match{
			Provider:   "openlibrary",
			ProviderID: "OL1M",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
			ISBN:       "0-441-17271-7",
		},
	}
	p := NewProviderWithSources([]Source{source})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-441-17271-9"},
	})

	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if match == nil || match.ProviderID != "OL1M" {
		t.Fatalf("Fetch() = %#v, want equivalent ISBN-10 match", match)
	}
}

func TestProviderFetchISBNRejectsContradictoryISBNFallback(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{
			id: "openlibrary",
			fetch: &metadata.Match{
				Provider:   "openlibrary",
				ProviderID: "OL-WRONG",
				Title:      "Dune",
				Authors:    []string{"Frank Herbert"},
				ISBN:       "978-0-593-13520-4",
			},
		},
		&fakeSource{id: "googlebooks"},
	})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-441-17271-9"},
	})

	if err != nil {
		t.Fatalf("Fetch() error = %v, want healthy no-match", err)
	}
	if match != nil {
		t.Fatalf("Fetch() = %#v, want contradictory ISBN rejected", match)
	}
}

func TestProviderFetchISBNAllowsFallbackWithStrongMetadataEvidence(t *testing.T) {
	p := NewProviderWithSources([]Source{
		&fakeSource{
			id: "openlibrary",
			fetch: &metadata.Match{
				Provider:   "openlibrary",
				ProviderID: "OL1M",
				Title:      "Dune",
				Authors:    []string{"Frank Herbert"},
			},
		},
	})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		Title:       "Dune",
		Authors:     []string{"Frank Herbert"},
		ProviderIDs: map[string]string{"isbn": "978-0-441-17271-9"},
	})

	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if match == nil || match.ProviderID != "OL1M" {
		t.Fatalf("Fetch() = %#v, want strong metadata fallback", match)
	}
}

func TestProviderFetchISBNFallbackDoesNotReturnFailureAfterHealthyNoMatch(t *testing.T) {
	openLibrary := &fakeSource{id: "openlibrary", fetchID: "9780593135204"}
	googleBooks := &fakeSource{id: "googlebooks", fetchID: "9780593135204", err: errors.New("temporary")}
	p := NewProviderWithSources([]Source{openLibrary, googleBooks})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-593-13520-4"},
	})
	if match != nil {
		t.Fatalf("Fetch() match = %#v, want nil", match)
	}
	if err != nil {
		t.Fatalf("Fetch() error = %v, want healthy no-match", err)
	}
}

func TestNewProviderWithOptionsFiltersEnabledSources(t *testing.T) {
	p := NewProviderWithOptions(Options{EnabledSources: []string{"openlibrary, googlebooks"}})

	if len(p.sources) != 2 {
		t.Fatalf("sources length = %d, want 2", len(p.sources))
	}
	if p.byID["openlibrary"] == nil || p.byID["googlebooks"] == nil {
		t.Fatalf("enabled sources missing from byID: %#v", p.byID)
	}
	if p.byID["amazon"] != nil {
		t.Fatalf("amazon source enabled unexpectedly")
	}
}

func TestNewProviderWithOptionsPassesAPIKeys(t *testing.T) {
	p := NewProviderWithOptions(Options{
		EnabledSources:    []string{"googlebooks,isbndb,hardcover"},
		GoogleBooksAPIKey: "google-key",
		ISBNdbAPIKey:      "isbn-key",
		HardcoverAPIKey:   "hardcover-key",
	})

	google, ok := p.byID["googlebooks"].(*GoogleBooksClient)
	if !ok || google.apiKey != "google-key" {
		t.Fatalf("GoogleBooks api key not configured")
	}
	isbndb, ok := p.byID["isbndb"].(*ISBNdbClient)
	if !ok || isbndb.apiKey != "isbn-key" {
		t.Fatalf("ISBNdb api key not configured")
	}
	hardcover, ok := p.byID["hardcover"].(*HardcoverClient)
	if !ok || hardcover.apiKey != "hardcover-key" {
		t.Fatalf("Hardcover api key not configured")
	}
}

func TestNewProviderDefaultsToReliableSources(t *testing.T) {
	p := NewProvider()

	got := sourceIDs(p.sources)
	want := []string{
		"openlibrary",
		"googlebooks",
		"isbndb",
		"hardcover",
	}
	if !slicesEqual(got, want) {
		t.Fatalf("default source IDs = %#v, want %#v", got, want)
	}
}

func TestNewProviderSpecializedSourcesRequireExplicitOptIn(t *testing.T) {
	p := NewProviderWithOptions(Options{
		EnabledSources: []string{"gutenberg,bookbrainz,internetarchive"},
	})

	got := sourceIDs(p.sources)
	want := []string{"gutenberg", "bookbrainz", "internetarchive"}
	if !slicesEqual(got, want) {
		t.Fatalf("explicit specialized source IDs = %#v, want %#v", got, want)
	}
	if len(p.catalog) != 0 || len(p.extended) != 3 {
		t.Fatalf("source tiers = catalog %d extended %d, want 0 and 3", len(p.catalog), len(p.extended))
	}
}

func TestNewProviderWithOptionsPreservesExplicitEnabledSources(t *testing.T) {
	p := NewProviderWithOptions(Options{
		EnabledSources: []string{"amazon, openlibrary"},
	})

	got := sourceIDs(p.sources)
	want := []string{"amazon", "openlibrary"}
	if !slicesEqual(got, want) {
		t.Fatalf("explicit source IDs = %#v, want %#v", got, want)
	}
}

func TestProviderSearchStopsAfterAcceptableCatalogMatch(t *testing.T) {
	catalog := &fakeSource{
		id: "openlibrary",
		search: []metadata.Match{{
			Provider:   "openlibrary",
			ProviderID: "OL1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
		}},
	}
	extended := &fakeSource{
		id: "amazon",
		search: []metadata.Match{{
			Provider:   "amazon",
			ProviderID: "A1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
		}},
	}
	p := NewProviderWithSources([]Source{catalog, extended})

	matches, err := p.Search(context.Background(), metadata.SearchQuery{
		Title:   "Dune",
		Authors: []string{"Frank Herbert"},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(matches) != 1 || matches[0].Provider != "openlibrary" {
		t.Fatalf("Search() = %#v, want catalog match", matches)
	}
	if extended.searchCalls != 0 {
		t.Fatalf("extended search calls = %d, want 0", extended.searchCalls)
	}
}

func TestProviderSearchFallsBackToExplicitExtendedSources(t *testing.T) {
	catalog := &fakeSource{
		id: "openlibrary",
		search: []metadata.Match{{
			Provider:   "openlibrary",
			ProviderID: "OL1",
			Title:      "Dune Messiah",
			Authors:    []string{"Frank Herbert"},
		}},
	}
	extended := &fakeSource{
		id: "amazon",
		search: []metadata.Match{{
			Provider:   "amazon",
			ProviderID: "A1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
		}},
	}
	p := NewProviderWithSources([]Source{catalog, extended})

	matches, err := p.Search(context.Background(), metadata.SearchQuery{
		Title:   "Dune",
		Authors: []string{"Frank Herbert"},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(matches) != 1 || matches[0].Provider != "amazon" {
		t.Fatalf("Search() = %#v, want extended fallback match", matches)
	}
	if catalog.searchCalls != 1 || extended.searchCalls != 1 {
		t.Fatalf("search calls = catalog %d, extended %d; want 1 each", catalog.searchCalls, extended.searchCalls)
	}
}

func TestProviderSearchTitleOnlyCatalogMatchDoesNotSuppressExtendedFallback(t *testing.T) {
	catalog := &fakeSource{
		id: "openlibrary",
		search: []metadata.Match{{
			Provider:   "openlibrary",
			ProviderID: "OL1",
			Title:      "Dune",
		}},
	}
	extended := &fakeSource{
		id: "amazon",
		search: []metadata.Match{{
			Provider:   "amazon",
			ProviderID: "A1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
		}},
	}
	p := NewProviderWithSources([]Source{catalog, extended})

	matches, err := p.Search(context.Background(), metadata.SearchQuery{
		Title:   "Dune",
		Authors: []string{"Frank Herbert"},
	})

	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if extended.searchCalls != 1 {
		t.Fatalf("extended search calls = %d, want 1", extended.searchCalls)
	}
	if len(matches) == 0 || matches[0].Provider != "amazon" {
		t.Fatalf("Search() = %#v, want strong extended match first", matches)
	}
}

func TestProviderSearchISBNFetchesSequentiallyAndStopsEarly(t *testing.T) {
	isbn := "9780593135204"
	first := &fakeSource{
		id:      "openlibrary",
		fetchID: isbn,
		fetch: &metadata.Match{
			Provider:   "openlibrary",
			ProviderID: isbn,
			Title:      "The Glass Hotel",
			Authors:    []string{"Emily St. John Mandel"},
			ISBN:       isbn,
		},
	}
	second := &fakeSource{
		id:      "googlebooks",
		fetchID: isbn,
		fetch: &metadata.Match{
			Provider:   "googlebooks",
			ProviderID: "GB1",
			Title:      "The Glass Hotel",
			Authors:    []string{"Emily St. John Mandel"},
			ISBN:       isbn,
		},
	}
	p := NewProviderWithSources([]Source{first, second})

	matches, err := p.Search(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-593-13520-4"},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(matches) != 1 || matches[0].Provider != "openlibrary" {
		t.Fatalf("Search() = %#v, want first identifier match", matches)
	}
	if len(first.fetched) != 1 {
		t.Fatalf("first source fetches = %#v, want one", first.fetched)
	}
	if len(second.fetched) != 0 {
		t.Fatalf("second source fetches = %#v, want none", second.fetched)
	}
}

func sourceIDs(sources []Source) []string {
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		ids = append(ids, source.ID())
	}
	return ids
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
