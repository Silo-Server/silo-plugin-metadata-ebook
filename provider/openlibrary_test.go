package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
	search := loadProviderFixture(t, "openlibrary_search.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/isbn/9780593135204"):
			w.Write(book)
		case strings.HasPrefix(r.URL.Path, "/isbn/9780000000000"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/books/OL27924614M"):
			w.Write(book)
		case r.URL.Path == "/search.json":
			w.Write(search)
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

func TestOpenLibrarySearchByText(t *testing.T) {
	srv, client := newOpenLibraryFake(t)
	defer srv.Close()

	matches, err := client.Search(context.Background(), metadata.SearchQuery{Title: "Project Hail Mary", Authors: []string{"Andy Weir"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("Search() returned %d matches, want 1", len(matches))
	}
	match := matches[0]
	if match.ProviderID != "9780593135204" {
		t.Fatalf("ProviderID = %q, want 9780593135204", match.ProviderID)
	}
	if match.Title != "Project Hail Mary" || match.ISBN != "9780593135204" || match.PublishYear != 2021 {
		t.Fatalf("Search()[0] = %#v", match)
	}
	selected, err := client.Fetch(context.Background(), match.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.Title != "Project Hail Mary" || selected.ProviderID != "OL27924614M" {
		t.Fatalf("Fetch(selected ProviderID) = %#v", selected)
	}
}

func TestOpenLibrarySearchDocUsesCoverEditionKey(t *testing.T) {
	// search.json docs carry a work key (/works/…W) and often no isbn; the
	// provider ID must fall back to cover_edition_key (an editable OL…M id),
	// otherwise the match is dropped and title searches return nothing.
	doc := openLibrarySearchDoc{
		Key:             "/works/OL20668819W",
		CoverEditionKey: "OL27924614M",
		Title:           "Project Hail Mary",
		CoverID:         11200092,
	}
	m := doc.toMatch("https://covers.openlibrary.org")
	if m.ProviderID != "OL27924614M" {
		t.Fatalf("ProviderID = %q, want OL27924614M (cover_edition_key)", m.ProviderID)
	}
	if m.CoverURL == "" {
		t.Fatalf("CoverURL empty, want cover from cover_i")
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
