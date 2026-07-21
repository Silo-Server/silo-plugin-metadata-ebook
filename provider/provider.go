package provider

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

const (
	providerTimeout            = 10 * time.Second
	metadataFetchRetryHeadroom = 2 * time.Second
	searchWorkers              = 4
	sourceCallConcurrency      = 4
	sourceAdmissionRetry       = time.Second
)

var ErrSourcesUnavailable = errors.New("ebook metadata sources unavailable")

type RateLimitedError struct {
	retryAfter time.Duration
	cause      error
}

func NewRateLimitedError(retryAfter time.Duration) *RateLimitedError {
	if retryAfter <= 0 {
		retryAfter = time.Second
	}
	return &RateLimitedError{
		retryAfter: retryAfter,
		cause:      ErrSourcesUnavailable,
	}
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("ebook metadata sources rate limited; retry after %s", e.retryAfter)
}

func (e *RateLimitedError) Unwrap() error {
	return e.cause
}

func (e *RateLimitedError) RetryAfter() time.Duration {
	return e.retryAfter
}

type Source interface {
	ID() string
	Search(context.Context, metadata.SearchQuery) ([]metadata.Match, error)
	Fetch(context.Context, string) (*metadata.Match, error)
}

type sourceEligibility interface {
	eligible() bool
}

type SourceTier string

const (
	SourceTierIdentifier SourceTier = "identifier"
	SourceTierCatalog    SourceTier = "catalog"
	SourceTierExtended   SourceTier = "extended"
)

type Provider struct {
	sources    []Source
	identifier []Source
	catalog    []Source
	extended   []Source
	byID       map[string]Source
	admission  map[string]chan struct{}
	health     *SourceHealth
	timeout    time.Duration
}

type Options struct {
	EnabledSources    []string
	GoogleBooksAPIKey string
	ISBNdbAPIKey      string
	HardcoverAPIKey   string
	DefaultRegion     string
}

type sourceBatch struct {
	matches    []metadata.Match
	successful int
	errs       []error
}

func NewProvider() *Provider {
	return NewProviderWithOptions(Options{})
}

func NewProviderWithOptions(options Options) *Provider {
	return NewProviderWithSources(defaultSources(options))
}

func NewProviderWithSources(sources []Source) *Provider {
	p := &Provider{
		sources:   make([]Source, 0, len(sources)),
		byID:      make(map[string]Source, len(sources)),
		admission: make(map[string]chan struct{}, len(sources)),
		health:    NewSourceHealth(),
		timeout:   providerTimeout,
	}

	for _, source := range sources {
		if source == nil {
			continue
		}
		id := strings.TrimSpace(source.ID())
		if id == "" {
			continue
		}
		p.sources = append(p.sources, source)
		p.byID[id] = source
		p.admission[strings.ToLower(id)] = make(chan struct{}, sourceCallConcurrency)
		if isIdentifierSource(id) {
			p.identifier = append(p.identifier, source)
		}
		if sourceTier(id) == SourceTierExtended {
			p.extended = append(p.extended, source)
		} else {
			p.catalog = append(p.catalog, source)
		}
	}

	return p
}

func (p *Provider) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	tctx, cancel := p.requestContext(ctx)
	defer cancel()

	if isbn := queryISBN(q); isbn != "" {
		batch := p.searchISBN(tctx, q, isbn)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := tctx.Err(); err != nil {
			if len(batch.matches) > 0 {
				return batch.matches, nil
			}
			return nil, err
		}
		if batch.successful == 0 {
			return nil, sourcesUnavailable("search", batch.errs)
		}
		return batch.matches, nil
	}

	catalog := p.searchSources(tctx, p.catalog, q)
	matches := RankMatches(q, catalog.matches)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := tctx.Err(); err != nil {
		if len(matches) > 0 {
			return matches, nil
		}
		return nil, err
	}
	if hasHighConfidenceMatch(q, matches) || len(p.extended) == 0 {
		if catalog.successful == 0 {
			return nil, sourcesUnavailable("search", catalog.errs)
		}
		return matches, nil
	}

	extended := p.searchSources(tctx, p.extended, q)
	allMatches := make([]metadata.Match, 0, len(catalog.matches)+len(extended.matches))
	allMatches = append(allMatches, catalog.matches...)
	allMatches = append(allMatches, extended.matches...)
	ranked := RankMatches(q, allMatches)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := tctx.Err(); err != nil {
		if len(ranked) > 0 {
			return ranked, nil
		}
		return nil, err
	}
	if catalog.successful+extended.successful == 0 {
		return nil, sourcesUnavailable("search", append(catalog.errs, extended.errs...))
	}
	return ranked, nil
}

func (p *Provider) searchSources(ctx context.Context, sources []Source, q metadata.SearchQuery) sourceBatch {
	type result struct {
		source  string
		matches []metadata.Match
		success bool
		err     error
	}

	results := make(chan result, len(sources))
	sem := make(chan struct{}, searchWorkers)

	var wg sync.WaitGroup
	for _, source := range sources {
		wg.Add(1)
		go func(source Source) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results <- result{source: strings.TrimSpace(source.ID()), err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			matches, success, err := p.searchSource(ctx, source, q)
			results <- result{
				source:  strings.TrimSpace(source.ID()),
				matches: matches,
				success: success,
				err:     err,
			}
		}(source)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var batch sourceBatch
	for result := range results {
		if result.err != nil {
			log.Printf("ebook-metadata: provider %s search error: %v", result.source, result.err)
			batch.errs = append(batch.errs, result.err)
			continue
		}
		if result.success {
			batch.successful++
			batch.matches = append(batch.matches, result.matches...)
		}
	}

	return batch
}

func (p *Provider) searchISBN(ctx context.Context, q metadata.SearchQuery, isbn string) sourceBatch {
	var batch sourceBatch
	var candidates []metadata.Match
	for _, source := range p.identifier {
		match, success, err := p.fetchSource(ctx, source, isbn)
		if err != nil {
			log.Printf("ebook-metadata: provider %s fetch error: %v", source.ID(), err)
			batch.errs = append(batch.errs, err)
			continue
		}
		if success {
			batch.successful++
		}
		if match == nil {
			continue
		}
		candidates = append(candidates, *match)
		if completeISBNMatch(*match, isbn) {
			batch.matches = candidates[len(candidates)-1:]
			return batch
		}
	}
	batch.matches = RankMatches(q, candidates)
	return batch
}

func (p *Provider) searchSource(ctx context.Context, source Source, q metadata.SearchQuery) ([]metadata.Match, bool, error) {
	sourceID := strings.TrimSpace(source.ID())
	if !sourceIsEligible(source) {
		return nil, false, nil
	}
	release, err := p.acquireSource(sourceID)
	if err != nil {
		return nil, false, err
	}
	defer release()
	if retryAfter, blocked := p.health.RetryAfter(sourceID); blocked {
		return nil, false, &sourceAdmissionError{cause: errSourceHealthCooldown, retryAfter: retryAfter}
	}
	matches, err := source.Search(ctx, q)
	if err != nil {
		var admissionErr *sourceAdmissionError
		if ctx.Err() == nil && !errors.As(err, &admissionErr) {
			p.health.RecordFailure(sourceID, err)
		}
		return nil, false, err
	}
	p.health.RecordSuccess(sourceID)
	return matches, true, nil
}

func (p *Provider) fetchSource(ctx context.Context, source Source, id string) (*metadata.Match, bool, error) {
	sourceID := strings.TrimSpace(source.ID())
	if !sourceIsEligible(source) {
		return nil, false, nil
	}
	release, err := p.acquireSource(sourceID)
	if err != nil {
		return nil, false, err
	}
	defer release()
	if retryAfter, blocked := p.health.RetryAfter(sourceID); blocked {
		return nil, false, &sourceAdmissionError{cause: errSourceHealthCooldown, retryAfter: retryAfter}
	}
	match, err := source.Fetch(ctx, id)
	if err != nil {
		var admissionErr *sourceAdmissionError
		if ctx.Err() == nil && !errors.As(err, &admissionErr) {
			p.health.RecordFailure(sourceID, err)
		}
		return nil, false, err
	}
	p.health.RecordSuccess(sourceID)
	return match, true, nil
}

func (p *Provider) acquireSource(sourceID string) (func(), error) {
	if p == nil {
		return nil, &sourceAdmissionError{
			cause:      errSourceAdmissionUnavailable,
			retryAfter: sourceAdmissionRetry,
		}
	}
	limit := p.admission[strings.ToLower(strings.TrimSpace(sourceID))]
	if limit == nil {
		return func() {}, nil
	}
	select {
	case limit <- struct{}{}:
		return func() { <-limit }, nil
	default:
		return nil, &sourceAdmissionError{
			cause:      errSourceAdmissionUnavailable,
			retryAfter: sourceAdmissionRetry,
		}
	}
}

func sourceIsEligible(source Source) bool {
	if eligibility, ok := source.(sourceEligibility); ok {
		return eligibility.eligible()
	}
	return true
}

func queryISBN(q metadata.SearchQuery) string {
	if isbn := metadata.NormalizeISBN(q.ProviderIDs["isbn"]); isbn != "" {
		return isbn
	}
	return metadata.NormalizeISBN(strings.TrimSpace(q.Title))
}

func completeISBNMatch(match metadata.Match, isbn string) bool {
	return metadata.EquivalentISBN(match.ISBN, isbn) &&
		strings.TrimSpace(match.Title) != "" &&
		len(match.Authors) > 0
}

func (p *Provider) Fetch(ctx context.Context, q metadata.SearchQuery) (*metadata.Match, error) {
	tctx, cancel := p.requestContext(ctx)
	defer cancel()

	for _, source := range p.sources {
		sourceID := strings.TrimSpace(source.ID())
		providerID := strings.TrimSpace(q.ProviderIDs[sourceID])
		if sourceID == "" || providerID == "" {
			continue
		}
		return p.fetchOneWithAdmissionRetry(tctx, source, providerID)
	}

	if sourceID, providerID := metadata.ParseCapabilityProviderID(q.ProviderIDs[metadata.CapabilityID]); sourceID != "" {
		if source := p.byID[sourceID]; source != nil {
			return p.fetchOneWithAdmissionRetry(tctx, source, providerID)
		}
	}

	isbn := metadata.NormalizeISBN(q.ProviderIDs["isbn"])
	if isbn == "" {
		return nil, nil
	}
	var successful int
	var errs []error
	var fallback *metadata.Match
	for _, source := range p.identifier {
		match, success, err := p.fetchSource(tctx, source, isbn)
		if success {
			successful++
		}
		if match != nil && safeISBNFallback(q, *match, isbn) {
			if completeISBNMatch(*match, isbn) {
				if contextErr := tctx.Err(); contextErr != nil {
					return nil, contextErr
				}
				return match, nil
			}
			if fallback == nil {
				fallback = match
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if err := tctx.Err(); err != nil {
		return nil, err
	}
	if fallback != nil {
		return fallback, nil
	}
	if successful == 0 {
		return nil, sourcesUnavailable("fetch", errs)
	}
	return nil, nil
}

func (p *Provider) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := p.timeout
	if timeout <= 0 {
		timeout = providerTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func safeISBNFallback(query metadata.SearchQuery, match metadata.Match, isbn string) bool {
	if matchISBN := metadata.NormalizeISBN(match.ISBN); matchISBN != "" {
		return metadata.EquivalentISBN(matchISBN, isbn)
	}
	if metadata.EquivalentISBN(match.ProviderID, isbn) {
		return true
	}
	return matchScore(query, match) >= matchConfidenceTitleContext
}

func (p *Provider) fetchOne(ctx context.Context, source Source, id string) (*metadata.Match, error) {
	match, success, err := p.fetchSource(ctx, source, id)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if success {
		return match, nil
	}
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	return nil, sourcesUnavailable("fetch", errs)
}

func (p *Provider) fetchOneWithAdmissionRetry(ctx context.Context, source Source, id string) (*metadata.Match, error) {
	match, err := p.fetchOne(ctx, source, id)
	if err == nil {
		return match, nil
	}
	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) || !errors.Is(err, errSourceAdmissionUnavailable) {
		return nil, err
	}
	retryAfter := rateLimited.RetryAfter()
	if retryAfter <= 0 {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok && retryAfter+metadataFetchRetryHeadroom >= time.Until(deadline) {
		return nil, err
	}
	timer := time.NewTimer(retryAfter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return p.fetchOne(ctx, source, id)
	}
}

func sourcesUnavailable(operation string, errs []error) error {
	if retryAfter, ok := allRateLimitRetryAfter(errs); ok {
		rateLimited := NewRateLimitedError(retryAfter)
		rateLimited.cause = errors.Join(ErrSourcesUnavailable, errors.Join(errs...))
		return fmt.Errorf("%s: %w", operation, rateLimited)
	}
	causes := make([]error, 1, len(errs)+1)
	causes[0] = ErrSourcesUnavailable
	causes = append(causes, errs...)
	return fmt.Errorf("%s: %w", operation, errors.Join(causes...))
}

func allRateLimitRetryAfter(errs []error) (time.Duration, bool) {
	if len(errs) == 0 {
		return 0, false
	}
	var earliest time.Duration
	for _, err := range errs {
		var admissionErr *sourceAdmissionError
		var retryAfter time.Duration
		switch {
		case errors.As(err, &admissionErr):
			retryAfter = admissionErr.RetryAfter()
		case statusCode(err) == 429:
			retryAfter = retryAfterDuration(err)
			if retryAfter <= 0 {
				retryAfter = healthRateLimitDefault
			}
		default:
			return 0, false
		}
		if retryAfter > 0 && (earliest == 0 || retryAfter < earliest) {
			earliest = retryAfter
		}
	}
	if earliest <= 0 {
		earliest = time.Second
	}
	return earliest, true
}

func defaultSources(options Options) []Source {
	userAgent := "silo-plugin-ebook-metadata/0.1"
	sources := []Source{
		NewOpenLibraryClient(userAgent),
		NewGoogleBooksClient(options.GoogleBooksAPIKey, userAgent),
		NewISBNdbClient(options.ISBNdbAPIKey, userAgent),
		NewHardcoverClient(options.HardcoverAPIKey, userAgent),
		NewGoodreadsClient(userAgent),
		NewAmazonClient(userAgent),
		NewAnnasArchiveClient(userAgent),
		NewGutenbergClient(userAgent),
		NewBookBrainzClient(userAgent),
		NewFantasticFictionClient(userAgent),
		NewISFDBClient(userAgent),
		NewLibraryThingClient(userAgent),
		NewInternetArchiveClient(userAgent),
		NewWorldCatClient(userAgent),
		NewDoubanClient(userAgent),
	}
	enabled := enabledSourceOrder(options.EnabledSources)
	if len(enabled) == 0 {
		filtered := make([]Source, 0, len(sources))
		for _, source := range sources {
			if source != nil && sourceTier(source.ID()) != SourceTierExtended {
				filtered = append(filtered, source)
			}
		}
		return filtered
	}
	available := make(map[string]Source, len(sources))
	for _, source := range sources {
		if source != nil {
			available[strings.ToLower(strings.TrimSpace(source.ID()))] = source
		}
	}
	filtered := make([]Source, 0, len(enabled))
	for _, sourceID := range enabled {
		if source := available[sourceID]; source != nil {
			filtered = append(filtered, source)
		}
	}
	return filtered
}

func sourceTier(sourceID string) SourceTier {
	switch strings.ToLower(strings.TrimSpace(sourceID)) {
	case "goodreads", "amazon", "annasarchive", "gutenberg", "bookbrainz", "fantasticfiction", "isfdb", "librarything", "internetarchive", "worldcat", "douban":
		return SourceTierExtended
	default:
		return SourceTierCatalog
	}
}

func isIdentifierSource(sourceID string) bool {
	switch strings.ToLower(strings.TrimSpace(sourceID)) {
	case "openlibrary", "googlebooks", "isbndb":
		return true
	default:
		return false
	}
}

func enabledSourceOrder(values []string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			if _, exists := seen[part]; exists {
				continue
			}
			seen[part] = struct{}{}
			out = append(out, part)
		}
	}
	return out
}
