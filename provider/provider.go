package provider

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

const (
	providerTimeout = 10 * time.Second
	// perSourceTimeout bounds one source within the sequential chain so a slow
	// source (e.g. a multi-fetch one) cannot starve the rest of the chain.
	perSourceTimeout = 6 * time.Second
	// chainTimeout is the overall budget for a Search across all chained sources.
	chainTimeout = 25 * time.Second
	// relevanceFloor is the minimum query-coverage score for a match to be kept;
	// it discards loose keyword hits (e.g. Gutenberg/Internet Archive returning
	// unrelated public-domain books or magazines for a title).
	relevanceFloor = 0.6
	// confidentScore is the coverage at which a covered match is trusted enough
	// to stop the chain, so later (rate-limited) sources are never queried.
	confidentScore = 0.85
)

type Source interface {
	ID() string
	Search(context.Context, metadata.SearchQuery) ([]metadata.Match, error)
	Fetch(context.Context, string) (*metadata.Match, error)
}

type Provider struct {
	sources []Source
	byID    map[string]Source
}

type Options struct {
	EnabledSources    []string
	GoogleBooksAPIKey string
	ISBNdbAPIKey      string
	HardcoverAPIKey   string
	DefaultRegion     string
}

func NewProvider() *Provider {
	return NewProviderWithOptions(Options{})
}

func NewProviderWithOptions(options Options) *Provider {
	return NewProviderWithSources(defaultSources(options))
}

func NewProviderWithSources(sources []Source) *Provider {
	p := &Provider{
		sources: make([]Source, 0, len(sources)),
		byID:    make(map[string]Source, len(sources)),
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
	}

	return p
}

// scoredMatch pairs a match with its query-coverage relevance score.
type scoredMatch struct {
	match metadata.Match
	score float64
}

// Search runs the sources as a relevance-ranked chain rather than a fanout.
// Sources are queried in priority order (cheap/unlimited first, rate-limited
// last); each match is scored for query coverage and only sufficiently relevant
// matches are kept. As soon as a source yields a confident, covered match the
// chain stops, so later sources — especially rate-limited ones like Hardcover —
// are queried only when earlier ones fall short. Results are returned best-first
// so the host's top-result consumer gets the correct book, not a loose hit that
// merely finished first.
func (p *Provider) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	cctx, cancel := context.WithTimeout(ctx, chainTimeout)
	defer cancel()

	var collected []scoredMatch
	for _, source := range p.sources {
		if cctx.Err() != nil {
			break
		}
		sctx, scancel := context.WithTimeout(cctx, perSourceTimeout)
		matches, err := source.Search(sctx, q)
		scancel()
		if err != nil {
			log.Printf("ebook-metadata: provider %s search error: %v", strings.TrimSpace(source.ID()), err)
			continue
		}

		confidentCovered := false
		for _, m := range matches {
			score := relevanceScore(q, m)
			if score < relevanceFloor {
				continue
			}
			collected = append(collected, scoredMatch{match: m, score: score})
			if score >= confidentScore && strings.TrimSpace(m.CoverURL) != "" {
				confidentCovered = true
			}
		}
		// A confident covered match ends the chain: later sources cannot improve
		// on it and need not spend their (possibly rate-limited) budget.
		if confidentCovered {
			break
		}
	}

	sort.SliceStable(collected, func(i, j int) bool {
		if collected[i].score != collected[j].score {
			return collected[i].score > collected[j].score
		}
		ci := strings.TrimSpace(collected[i].match.CoverURL) != ""
		cj := strings.TrimSpace(collected[j].match.CoverURL) != ""
		return ci && !cj
	})

	out := make([]metadata.Match, len(collected))
	for i := range collected {
		out[i] = collected[i].match
	}
	return out, nil
}

func (p *Provider) Fetch(ctx context.Context, q metadata.SearchQuery) (*metadata.Match, error) {
	tctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	match, backfill, err := p.fetchPrimary(tctx, q)
	// Backfill only on the ISBN-fallback path: an explicit source-specific or
	// capability ID pins a single source and must not fan out to others.
	if match != nil && backfill {
		p.backfillCover(tctx, match, q)
	}
	return match, err
}

// fetchPrimary resolves the best metadata match using provider-specific IDs,
// then the capability ID, then ISBN. It returns the first source that responds
// regardless of whether that match carries a cover. The bool reports whether
// the match came from the ISBN fallback (i.e. no explicit source pin), in which
// case the caller may backfill a missing cover from other sources.
func (p *Provider) fetchPrimary(tctx context.Context, q metadata.SearchQuery) (*metadata.Match, bool, error) {
	for _, source := range p.sources {
		sourceID := strings.TrimSpace(source.ID())
		providerID := strings.TrimSpace(q.ProviderIDs[sourceID])
		if sourceID == "" || providerID == "" {
			continue
		}
		match, err := source.Fetch(tctx, providerID)
		return match, false, err
	}

	if sourceID, providerID := metadata.ParseCapabilityProviderID(q.ProviderIDs[metadata.CapabilityID]); sourceID != "" {
		if source := p.byID[sourceID]; source != nil {
			match, err := source.Fetch(tctx, providerID)
			return match, false, err
		}
	}

	isbn := metadata.NormalizeISBN(q.ProviderIDs["isbn"])
	if isbn == "" {
		return nil, false, nil
	}
	var lastErr error
	for _, sourceID := range []string{"openlibrary", "googlebooks", "isbndb"} {
		source := p.byID[sourceID]
		if source == nil {
			continue
		}
		match, err := source.Fetch(tctx, isbn)
		if match != nil {
			return match, true, nil
		}
		if err != nil {
			lastErr = err
		}
	}

	return nil, false, lastErr
}

// backfillCover fills a missing cover from other ISBN-capable sources. The
// primary match often has full text metadata but no cover image (e.g. an
// OpenLibrary record with no cover), and the host has no GetImages fallback for
// this plugin, so the cover must travel on the match itself. We graft a cover
// from the first source that has one for this ISBN.
//
// ponytail: sequential extra fetches, only when the primary match has no
// cover; parallelize if this becomes a latency hotspot.
func (p *Provider) backfillCover(ctx context.Context, match *metadata.Match, q metadata.SearchQuery) {
	if match == nil || strings.TrimSpace(match.CoverURL) != "" {
		return
	}
	isbn := metadata.NormalizeISBN(q.ProviderIDs["isbn"])
	if isbn == "" {
		isbn = metadata.NormalizeISBN(match.ISBN)
	}
	if isbn == "" {
		return
	}
	for _, sourceID := range []string{"googlebooks", "openlibrary", "isbndb"} {
		if sourceID == match.Provider {
			continue // already produced this match and gave no cover
		}
		source := p.byID[sourceID]
		if source == nil {
			continue
		}
		alt, err := source.Fetch(ctx, isbn)
		if err != nil || alt == nil {
			continue
		}
		if cover := strings.TrimSpace(alt.CoverURL); cover != "" {
			match.CoverURL = cover
			return
		}
	}
}

func defaultSources(options Options) []Source {
	userAgent := "silo-plugin-ebook-metadata/0.1"
	// Ordered fast structured-API sources first, fragile HTML scrapers last.
	// Search fans out concurrently and ranks the aggregate, but this order
	// gives the fast, high-hit-rate APIs first claim on worker slots and keeps
	// the slow scrapers (which only matter for the long-tail) from starving
	// them. Fetch's ISBN fallback and cover backfill rely on the fast tier too.
	// Chain order: cheap/unlimited and high-precision sources first so the chain
	// usually stops early; rate-limited or quota-bound sources (Google Books,
	// Hardcover) sit last so they are only queried when earlier sources miss,
	// preserving their request budgets. Disabled-by-default scrapers trail.
	sources := []Source{
		NewOpenLibraryClient(userAgent),
		NewBookInfoClient(userAgent),
		NewInternetArchiveClient(userAgent),
		NewGutenbergClient(userAgent),
		NewGoogleBooksClient(options.GoogleBooksAPIKey, userAgent),
		NewHardcoverClient(options.HardcoverAPIKey, userAgent),
		NewISBNdbClient(options.ISBNdbAPIKey, userAgent),
		NewBookBrainzClient(userAgent),
		NewGoodreadsClient(userAgent),
		NewLibraryThingClient(userAgent),
		NewISFDBClient(userAgent),
		NewFantasticFictionClient(userAgent),
		NewAnnasArchiveClient(userAgent),
		NewAmazonClient(userAgent),
		NewWorldCatClient(userAgent),
		NewDoubanClient(userAgent),
	}
	enabled := enabledSourceSet(options.EnabledSources)
	if len(enabled) == 0 {
		return sources
	}
	filtered := make([]Source, 0, len(sources))
	for _, source := range sources {
		if source == nil {
			continue
		}
		if enabled[strings.TrimSpace(source.ID())] {
			filtered = append(filtered, source)
		}
	}
	return filtered
}

func enabledSourceSet(values []string) map[string]bool {
	out := make(map[string]bool)
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part != "" {
				out[part] = true
			}
		}
	}
	return out
}
