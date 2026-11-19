package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

func loadProviderFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func newOpenLibraryFake(t *testing.T) (*httptest.Server, *OpenLibraryClient) {
	t.Helper()
	book := loadProviderFixture(t, "openlibrary_book.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/isbn/9780593135204"):
			w.Write(book)
		case strings.HasPrefix(r.URL.Path, "/isbn/9780000000000"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/books/OL27924614M"):
			w.Write(book)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	client := NewOpenLibraryClientAt(srv.URL, "https://covers.openlibrary.org", "test-agent")
	client.client = srv.Client()
	return srv, client
}

func TestOpenLibraryFetchByISBN(t *testing.T) {
	srv, client := newOpenLibraryFake(t)
	defer srv.Close()

	match, err := client.Fetch(context.Background(), "978-0-593-13520-4")
	if err != nil {
		t.Fatal(err)
	}
	if match == nil {
		t.Fatal("Fetch() returned nil")
	}
	if match.Provider != "openlibrary" {
		t.Fatalf("Provider = %q, want openlibrary", match.Provider)
	}
	if match.ProviderID != "OL27924614M" {
		t.Fatalf("ProviderID = %q, want OL27924614M", match.ProviderID)
	}
	if match.Title != "Project Hail Mary" {
		t.Fatalf("Title = %q, want Project Hail Mary", match.Title)
	}
	if match.ISBN != "9780593135204" {
		t.Fatalf("ISBN = %q, want 9780593135204", match.ISBN)
	}
	if match.PublishYear != 2021 {
		t.Fatalf("PublishYear = %d, want 2021", match.PublishYear)
	}
	if match.Publisher != "Ballantine Books" || match.Language != "eng" || match.PageCount != 476 {
		t.Fatalf("mapped fields = %#v", match)
	}
	if len(match.Authors) != 1 || match.Authors[0] != "Andy Weir" {
		t.Fatalf("Authors = %#v, want Andy Weir", match.Authors)
	}
	if match.CoverURL != "https://covers.openlibrary.org/b/id/10473226-L.jpg" {
		t.Fatalf("CoverURL = %q", match.CoverURL)
	}
}

func TestOpenLibraryEditionAuthorRefsDoNotPretendNames(t *testing.T) {
	body := []byte(`{
		"key": "/books/OL1M",
		"title": "Referenced Authors",
		"authors": [{"key": "/authors/OL23919A"}],
		"isbn_13": ["9780593135204"]
	}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	client := NewOpenLibraryClientAt(srv.URL, "https://covers.openlibrary.org", "test-agent")
	client.client = srv.Client()
	match, err := client.Fetch(context.Background(), "978-0-593-13520-4")
	if err != nil {
		t.Fatal(err)
	}
	if match == nil {
		t.Fatal("Fetch() returned nil")
	}
	if len(match.Authors) != 0 {
		t.Fatalf("Authors = %#v, want empty until author refs are resolved", match.Authors)
	}
}

func TestOpenLibraryFetchMissing(t *testing.T) {
	srv, client := newOpenLibraryFake(t)
	defer srv.Close()

	match, err := client.Fetch(context.Background(), "9780000000000")
	if err != nil {
		t.Fatalf("Fetch() error = %v, want nil", err)
	}
	if match != nil {
		t.Fatalf("Fetch() = %#v, want nil", match)
	}
}

// newOpenLibraryCaptureFake serves two responses captured from openlibrary.org
// on 2026-11-18: the search for "Project Hail Mary Andy Weir" with the field
// list in openLibrarySearchFields, and the edition record its first doc names.
// Both files are the live bodies, reindented and otherwise unchanged.
func newOpenLibraryCaptureFake(t *testing.T, onRequest func(*http.Request)) *OpenLibraryClient {
	t.Helper()
	search := loadProviderFixture(t, "openlibrary_search.json")
	edition := loadProviderFixture(t, "openlibrary_edition_OL29597011M.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onRequest != nil {
			onRequest(r)
		}
		switch r.URL.Path {
		case "/search.json":
			w.Write(search)
		case "/books/OL29597011M.json":
			w.Write(edition)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client := NewOpenLibraryClientAt(srv.URL, "https://covers.openlibrary.org", "test-agent")
	client.client = srv.Client()
	return client
}

func TestOpenLibrarySearchByText(t *testing.T) {
	var searchQuery url.Values
	client := newOpenLibraryCaptureFake(t, func(r *http.Request) {
		if r.URL.Path == "/search.json" {
			searchQuery = r.URL.Query()
		}
	})

	matches, err := client.Search(context.Background(), metadata.SearchQuery{Title: "Project Hail Mary", Authors: []string{"Andy Weir"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := searchQuery.Get("q"); got != "Project Hail Mary Andy Weir" {
		t.Fatalf("q = %q, want the fixture's query", got)
	}
	// The fixture is the live response to exactly this field list. Capture it
	// again when the list changes: without "key" the service sent every doc an
	// empty editions list, and without "editions" it sends no edition at all.
	if got, want := searchQuery.Get("fields"), "key,title,author_name,first_publish_year,cover_i,editions,editions.key"; got != want {
		t.Fatalf("fields = %q, want %q", got, want)
	}
	if len(matches) != 8 {
		t.Fatalf("Search() returned %d matches, want all 8 docs in the fixture", len(matches))
	}
	for _, match := range matches {
		if !openLibraryEditionIDRE.MatchString(match.ProviderID) {
			t.Errorf("ProviderID = %q for %q, want an edition ID", match.ProviderID, match.Title)
		}
		if match.ISBN != "" {
			t.Errorf("ISBN = %q for %q, want none from a work-level doc", match.ISBN, match.Title)
		}
	}
	match := matches[0]
	if match.ProviderID != "OL29597011M" || match.Title != "Project Hail Mary" || match.PublishYear != 2021 {
		t.Fatalf("Search()[0] = %#v", match)
	}
	if len(match.Authors) != 1 || match.Authors[0] != "Andy Weir" {
		t.Fatalf("Authors = %#v, want Andy Weir", match.Authors)
	}
	if match.CoverURL != "https://covers.openlibrary.org/b/id/11200092-L.jpg" {
		t.Fatalf("CoverURL = %q", match.CoverURL)
	}

	selected, err := client.Fetch(context.Background(), match.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.ProviderID != match.ProviderID {
		t.Fatalf("Fetch(selected ProviderID) = %#v, want the same edition", selected)
	}
	if selected.ISBN != "9780593395561" {
		t.Fatalf("fetched ISBN = %q, want the edition's own 9780593395561", selected.ISBN)
	}
}

func TestOpenLibrarySearchDropsDocsWithoutEdition(t *testing.T) {
	// The first doc has the shape the service sends when it matches no edition,
	// the second the shape it sends when editions are not requested.
	body := []byte(`{"docs": [
		{"key": "/works/OL21745884W", "title": "Project Hail Mary", "first_publish_year": 2021,
			"editions": {"numFound": 0, "start": 0, "numFoundExact": true, "docs": []}},
		{"key": "/works/OL36735881W", "title": "The Martian / Artemis / Project Hail Mary", "first_publish_year": 2022}
	]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	client := NewOpenLibraryClientAt(srv.URL, "https://covers.openlibrary.org", "test-agent")
	client.client = srv.Client()
	matches, err := client.Search(context.Background(), metadata.SearchQuery{Title: "Project Hail Mary"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("Search() = %#v, want no match for a doc without an edition", matches)
	}
}

func TestOpenLibrarySearchUsesItemISBNOverText(t *testing.T) {
	// The fake has no /search.json route, so a text search would fail.
	srv, client := newOpenLibraryFake(t)
	defer srv.Close()

	query := metadata.SearchQuery{
		Title:       "Project Hail Mary Andy Weir",
		ProviderIDs: map[string]string{"isbn": "9780593135204"},
	}
	matches, err := client.Search(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].ProviderID != "OL27924614M" || matches[0].ISBN != "9780593135204" {
		t.Fatalf("Search() = %#v, want the edition that holds the item's ISBN", matches)
	}

	// A valid ISBN the fake answers with 404.
	query.ProviderIDs["isbn"] = "9780593135211"
	matches, err = client.Search(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("Search() = %#v, want no match for an ISBN OpenLibrary does not have", matches)
	}
}

func TestOpenLibraryRequestsWaitForBudget(t *testing.T) {
	var requests atomic.Int32
	client := newOpenLibraryCaptureFake(t, func(*http.Request) { requests.Add(1) })
	client.client = &http.Client{
		Transport: rateLimitedTransport{limiter: newLimiter(openLibraryRequestsPerMinute)},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := client.Search(ctx, metadata.SearchQuery{Title: "Project Hail Mary"}); err != nil {
		t.Fatal(err)
	}
	// The search spent the budget. The next slot is a second away, past the
	// deadline, so the fetch must be refused without reaching the service.
	if match, err := client.Fetch(ctx, "OL29597011M"); err == nil {
		t.Fatalf("Fetch() = %#v, want a rate limit error", match)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("service saw %d requests, want 1", got)
	}
}

func TestNewOpenLibraryClientSharesOneRequestBudget(t *testing.T) {
	first, second := NewOpenLibraryClient("test-agent"), NewOpenLibraryClient("test-agent")
	if first.client != second.client {
		t.Fatal("clients do not share an HTTP client")
	}
	limiter := first.client.Transport.(rateLimitedTransport).limiter
	if limit, burst := limiter.Limit(), limiter.Burst(); limit != 1 || burst != 1 {
		t.Fatalf("limiter = %v requests a second with burst %d, want 1 and 1", limit, burst)
	}
}

func TestOpenLibrarySearchByISBN(t *testing.T) {
	srv, client := newOpenLibraryFake(t)
	defer srv.Close()

	matches, err := client.Search(context.Background(), metadata.SearchQuery{Title: "978-0-593-13520-4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("Search() returned %d matches, want 1", len(matches))
	}
	if matches[0].ProviderID != "OL27924614M" {
		t.Fatalf("ProviderID = %q, want OL27924614M", matches[0].ProviderID)
	}
}

func TestOpenLibraryUnrecognizedID(t *testing.T) {
	srv, client := newOpenLibraryFake(t)
	defer srv.Close()

	match, err := client.Fetch(context.Background(), "not-an-id")
	if err != nil {
		t.Fatalf("Fetch() error = %v, want nil", err)
	}
	if match != nil {
		t.Fatalf("Fetch() = %#v, want nil", match)
	}
}
