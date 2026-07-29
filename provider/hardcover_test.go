package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

func newHardcoverFake(t *testing.T, apiKey string) (*httptest.Server, *HardcoverClient, *int) {
	t.Helper()
	book := loadProviderFixture(t, "hardcover_book.json")
	search := loadProviderFixture(t, "hardcover_search.json")
	missing := []byte(`{"data":{"books_by_pk":null}}`)
	requests := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(string(body), "books_by_pk") && strings.Contains(string(body), `"id":97844`):
			w.Write(book)
		case strings.Contains(string(body), "books_by_pk"):
			w.Write(missing)
		default:
			w.Write(search)
		}
	}))
	client := NewHardcoverClientAt(srv.URL, apiKey, "test-agent")
	client.client = srv.Client()
	return srv, client, &requests
}

func TestHardcoverNoKeySearchFetchReturnNil(t *testing.T) {
	srv, client, requests := newHardcoverFake(t, "")
	defer srv.Close()

	matches, err := client.Search(context.Background(), metadata.SearchQuery{Title: "Project Hail Mary"})
	if err != nil || matches != nil {
		t.Fatalf("Search() = %#v, %v; want nil, nil", matches, err)
	}
	match, err := client.Fetch(context.Background(), "97844")
	if err != nil || match != nil {
		t.Fatalf("Fetch() = %#v, %v; want nil, nil", match, err)
	}
	if *requests != 0 {
		t.Fatalf("made %d requests without key, want 0", *requests)
	}
}

func TestHardcoverFetchByID(t *testing.T) {
	srv, client, _ := newHardcoverFake(t, "test-key")
	defer srv.Close()

	match, err := client.Fetch(context.Background(), "97844")
	if err != nil {
		t.Fatal(err)
	}
	if match == nil {
		t.Fatal("Fetch() returned nil")
	}
	if match.Provider != "hardcover" || match.ProviderID != "97844" {
		t.Fatalf("provider fields = %#v", match)
	}
	if match.Title != "Project Hail Mary" || match.ISBN != "9780593135204" || match.PublishYear != 2021 {
		t.Fatalf("mapped fields = %#v", match)
	}
	if len(match.Authors) != 1 || match.Authors[0] != "Andy Weir" {
		t.Fatalf("Authors = %#v", match.Authors)
	}
	if match.CoverURL != "https://example/cover.jpg" || match.PageCount != 476 {
		t.Fatalf("mapped fields = %#v", match)
	}
}

func TestHardcoverFetchMissing(t *testing.T) {
	srv, client, _ := newHardcoverFake(t, "test-key")
	defer srv.Close()

	match, err := client.Fetch(context.Background(), "99999")
	if err != nil {
		t.Fatalf("Fetch() error = %v, want nil", err)
	}
	if match != nil {
		t.Fatalf("Fetch() = %#v, want nil", match)
	}
}

func TestHardcoverSearchByText(t *testing.T) {
	srv, client, _ := newHardcoverFake(t, "test-key")
	defer srv.Close()

	matches, err := client.Search(context.Background(), metadata.SearchQuery{Title: "Project Hail Mary"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("Search() returned %d matches, want 2", len(matches))
	}
	first := matches[0]
	if first.Provider != "hardcover" || first.ProviderID != "97844" {
		t.Fatalf("Search()[0] = %#v", first)
	}
	if first.Title != "Project Hail Mary" || first.PublishYear != 2021 || first.PageCount != 476 {
		t.Fatalf("Search()[0] mapped = %#v", first)
	}
	if len(first.Authors) != 1 || first.Authors[0] != "Andy Weir" {
		t.Fatalf("Search()[0].Authors = %#v", first.Authors)
	}
	if first.ISBN != "9780593135204" {
		t.Fatalf("Search()[0].ISBN = %q, want the 13-digit form preferred", first.ISBN)
	}
	if first.CoverURL != "https://example/cover.jpg" {
		t.Fatalf("Search()[0].CoverURL = %q", first.CoverURL)
	}
	if matches[1].ProviderID != "555001" || matches[1].ISBN != "" {
		t.Fatalf("Search()[1] = %#v", matches[1])
	}
}

func TestHardcoverFetchNonnumericReturnsNil(t *testing.T) {
	srv, client, requests := newHardcoverFake(t, "test-key")
	defer srv.Close()

	match, err := client.Fetch(context.Background(), "not-a-number")
	if err != nil {
		t.Fatalf("Fetch() error = %v, want nil", err)
	}
	if match != nil {
		t.Fatalf("Fetch() = %#v, want nil", match)
	}
	if *requests != 0 {
		t.Fatalf("made %d requests for nonnumeric ID, want 0", *requests)
	}
}

func TestHardcoverSaturatedLimiterWaitsBoundedThenCallsUpstream(t *testing.T) {
	// Hardcover documents a hard 60rpm limit; the client admits at 50rpm so a
	// saturated worker pool sleeps briefly between calls instead of blasting
	// past the ceiling into 429s and Cloudflare 403 blocks.
	srv, client, calls := newHardcoverFake(t, "key")
	defer srv.Close()

	if _, err := client.Fetch(context.Background(), "42"); err != nil {
		t.Fatalf("first Fetch() error = %v", err)
	}

	started := time.Now()
	if _, err := client.Fetch(context.Background(), "42"); err != nil {
		t.Fatalf("second Fetch() error = %v, want success after bounded wait", err)
	}
	elapsed := time.Since(started)
	if elapsed < 500*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("second Fetch() took %s, want ~1.2s bounded token wait", elapsed)
	}
	if *calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 after bounded wait admission", *calls)
	}
}
