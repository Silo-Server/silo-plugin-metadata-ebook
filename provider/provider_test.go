package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

type fakeSource struct {
	id      string
	search  []metadata.Match
	fetch   *metadata.Match
	fetchID string
	err     error
	fetched []string
}

func (s fakeSource) ID() string {
	return s.id
}

func (s fakeSource) Search(context.Context, metadata.SearchQuery) ([]metadata.Match, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.search, nil
}

func (s *fakeSource) Fetch(_ context.Context, id string) (*metadata.Match, error) {
	s.fetched = append(s.fetched, id)
	if s.err != nil {
		return nil, s.err
	}
	if s.fetchID != "" && id != s.fetchID {
		return nil, nil
	}
	return s.fetch, nil
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

func TestProviderFetchBackfillsMissingCoverOnISBNFallback(t *testing.T) {
	// OpenLibrary wins the ISBN fallback with full metadata but no cover; the
	// cover must be grafted from a later source rather than left empty.
	openLibrary := &fakeSource{
		id:      "openlibrary",
		fetchID: "9780593135204",
		fetch:   &metadata.Match{Provider: "openlibrary", ProviderID: "9780593135204", Title: "Book"},
	}
	googleBooks := &fakeSource{
		id:      "googlebooks",
		fetchID: "9780593135204",
		fetch:   &metadata.Match{Provider: "googlebooks", CoverURL: "https://books.google.com/x.jpg"},
	}
	p := NewProviderWithSources([]Source{openLibrary, googleBooks})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-593-13520-4"},
	})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if match == nil || match.Provider != "openlibrary" {
		t.Fatalf("Fetch() = %#v, want openlibrary primary match", match)
	}
	if match.CoverURL != "https://books.google.com/x.jpg" {
		t.Fatalf("CoverURL = %q, want backfilled googlebooks cover", match.CoverURL)
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

func TestProviderFetchISBNFallbackReturnsLastErrorWhenNoSourceMatches(t *testing.T) {
	openLibrary := &fakeSource{id: "openlibrary", fetchID: "9780593135204"}
	googleBooks := &fakeSource{id: "googlebooks", fetchID: "9780593135204", err: errors.New("temporary")}
	p := NewProviderWithSources([]Source{openLibrary, googleBooks})

	match, err := p.Fetch(context.Background(), metadata.SearchQuery{
		ProviderIDs: map[string]string{"isbn": "978-0-593-13520-4"},
	})
	if match != nil {
		t.Fatalf("Fetch() match = %#v, want nil", match)
	}
	if err == nil {
		t.Fatal("Fetch() error = nil, want last fallback error")
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
