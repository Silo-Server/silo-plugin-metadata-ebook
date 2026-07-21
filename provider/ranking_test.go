package provider

import (
	"reflect"
	"testing"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

func TestRankMatchesOrdersConfidenceLevels(t *testing.T) {
	query := metadata.SearchQuery{
		Title:       "The Left Hand of Darkness",
		Authors:     []string{"Ursula K. Le Guin"},
		Year:        1969,
		Language:    "en",
		ProviderIDs: map[string]string{"isbn": "978-0-441-47812-5"},
	}
	matches := []metadata.Match{
		{
			Provider:    "catalog",
			ProviderID:  "year-language",
			Title:       "The Left Hand of Darkness",
			PublishYear: 1969,
			Language:    "en",
		},
		{
			Provider:   "catalog",
			ProviderID: "title-author",
			Title:      "the left hand of darkness",
			Authors:    []string{"Ursula K Le Guin"},
		},
		{
			Provider:   "identifier",
			ProviderID: "isbn",
			Title:      "Different Catalog Title",
			ISBN:       "9780441478125",
		},
	}

	got := RankMatches(query, matches)
	gotIDs := matchIDs(got)
	wantIDs := []string{"isbn", "title-author", "year-language"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("RankMatches() IDs = %#v, want %#v", gotIDs, wantIDs)
	}
}

func TestRankMatchesIsDeterministicForEqualConfidence(t *testing.T) {
	query := metadata.SearchQuery{
		Title:   "Dune",
		Authors: []string{"Frank Herbert"},
	}
	left := metadata.Match{
		Provider:   "amazon",
		ProviderID: "B",
		Title:      "Dune",
		Authors:    []string{"Frank Herbert"},
	}
	right := metadata.Match{
		Provider:   "openlibrary",
		ProviderID: "A",
		Title:      "Dune",
		Authors:    []string{"Frank Herbert"},
	}

	first := matchIDs(RankMatches(query, []metadata.Match{right, left}))
	second := matchIDs(RankMatches(query, []metadata.Match{left, right}))
	want := []string{"B", "A"}
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Fatalf("deterministic IDs = %#v and %#v, want %#v", first, second, want)
	}
}

func TestRankMatchesDeduplicatesNormalizedISBN(t *testing.T) {
	query := metadata.SearchQuery{
		Title:   "Dune",
		Authors: []string{"Frank Herbert"},
	}
	matches := []metadata.Match{
		{
			Provider:   "openlibrary",
			ProviderID: "OL1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
			ISBN:       "978-0-441-17271-9",
		},
		{
			Provider:   "googlebooks",
			ProviderID: "GB1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
			ISBN:       "9780441172719",
		},
	}

	got := RankMatches(query, matches)
	if len(got) != 1 {
		t.Fatalf("RankMatches() length = %d, want 1: %#v", len(got), got)
	}
	if got[0].Provider != "googlebooks" {
		t.Fatalf("deduplicated provider = %q, want deterministic googlebooks winner", got[0].Provider)
	}
}

func TestRankMatchesDeduplicatesEquivalentISBN10AndISBN13(t *testing.T) {
	query := metadata.SearchQuery{Title: "Dune", Authors: []string{"Frank Herbert"}}
	matches := []metadata.Match{
		{
			Provider:   "openlibrary",
			ProviderID: "OL1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
			ISBN:       "0-441-17271-7",
		},
		{
			Provider:   "googlebooks",
			ProviderID: "GB1",
			Title:      "Dune",
			Authors:    []string{"Frank Herbert"},
			ISBN:       "978-0-441-17271-9",
		},
	}

	got := RankMatches(query, matches)

	if len(got) != 1 {
		t.Fatalf("RankMatches() length = %d, want one edition: %#v", len(got), got)
	}
}

func TestRankMatchesTreatsEquivalentISBNAsExact(t *testing.T) {
	query := metadata.SearchQuery{ProviderIDs: map[string]string{"isbn": "0-441-17271-7"}}
	match := metadata.Match{
		Provider:   "googlebooks",
		ProviderID: "GB1",
		Title:      "Dune",
		ISBN:       "978-0-441-17271-9",
	}

	got := RankMatches(query, []metadata.Match{match})

	if len(got) != 1 {
		t.Fatalf("RankMatches() = %#v, want equivalent ISBN match", got)
	}
}

func TestRankMatchesDeduplicatesSourceProviderID(t *testing.T) {
	query := metadata.SearchQuery{Title: "Dune"}
	match := metadata.Match{
		Provider:   "openlibrary",
		ProviderID: "OL1",
		Title:      "Dune",
	}

	got := RankMatches(query, []metadata.Match{match, match})
	if len(got) != 1 {
		t.Fatalf("RankMatches() length = %d, want 1", len(got))
	}
}

func TestRankMatchesRejectsLowConfidenceTitleOnlyCollisions(t *testing.T) {
	query := metadata.SearchQuery{Title: "Foundation"}
	matches := []metadata.Match{
		{
			Provider:   "one",
			ProviderID: "1",
			Title:      "Foundation",
			Authors:    []string{"Isaac Asimov"},
		},
		{
			Provider:   "two",
			ProviderID: "2",
			Title:      "Foundation",
			Authors:    []string{"Peter Ackroyd"},
		},
	}

	got := RankMatches(query, matches)
	if len(got) != 0 {
		t.Fatalf("RankMatches() = %#v, want ambiguous title-only matches rejected", got)
	}
}

func TestRankMatchesRejectsContradictoryEvidence(t *testing.T) {
	query := metadata.SearchQuery{
		Title:    "Dune",
		Authors:  []string{"Frank Herbert"},
		Year:     1965,
		Language: "en",
	}
	matches := []metadata.Match{
		{Provider: "catalog", ProviderID: "author", Title: "Dune", Authors: []string{"Brian Herbert"}, PublishYear: 1965, Language: "en"},
		{Provider: "catalog", ProviderID: "year", Title: "Dune", Authors: []string{"Frank Herbert"}, PublishYear: 2020, Language: "en"},
		{Provider: "catalog", ProviderID: "language", Title: "Dune", Authors: []string{"Frank Herbert"}, PublishYear: 1965, Language: "fr"},
		{Provider: "catalog", ProviderID: "compatible", Title: "Dune", Authors: []string{"Frank Herbert"}, PublishYear: 1965, Language: "en"},
	}

	got := RankMatches(query, matches)

	if ids := matchIDs(got); !reflect.DeepEqual(ids, []string{"compatible"}) {
		t.Fatalf("RankMatches() IDs = %#v, want only compatible evidence", ids)
	}
}

func TestRankMatchesUnderstandsFreeTextTitleAndAuthorQuery(t *testing.T) {
	match := metadata.Match{
		Provider: "catalog", ProviderID: "right", Title: "Dune",
		Authors: []string{"Frank Herbert"},
	}

	for _, query := range []string{"Dune Herbert", "Frank Herbert Dune", "Dune Frank Herbert"} {
		got := RankMatches(metadata.SearchQuery{Title: query}, []metadata.Match{match})
		if ids := matchIDs(got); !reflect.DeepEqual(ids, []string{"right"}) {
			t.Fatalf("RankMatches(%q) IDs = %#v, want free-text title-author match", query, ids)
		}
	}
}

func TestRankMatchesUnderstandsPartialFreeTextTitles(t *testing.T) {
	match := metadata.Match{
		Provider: "catalog", ProviderID: "left-hand", Title: "The Left Hand of Darkness",
		Authors: []string{"Ursula K. Le Guin"},
	}

	for _, query := range []string{"Left Hand", "Ursula Left Hand", "Left Hand Le Guin"} {
		got := RankMatches(metadata.SearchQuery{Title: query}, []metadata.Match{match})
		if ids := matchIDs(got); !reflect.DeepEqual(ids, []string{"left-hand"}) {
			t.Fatalf("RankMatches(%q) IDs = %#v, want partial-title match", query, ids)
		}
	}
}

func TestRankMatchesRejectsUnrelatedFreeTextTerms(t *testing.T) {
	matches := []metadata.Match{
		{Provider: "catalog", ProviderID: "dune", Title: "Dune", Authors: []string{"Frank Herbert"}},
		{Provider: "catalog", ProviderID: "messiah", Title: "Dune Messiah", Authors: []string{"Frank Herbert"}},
	}

	got := RankMatches(metadata.SearchQuery{Title: "Dune Brian"}, matches)

	if len(got) != 0 {
		t.Fatalf("RankMatches() = %#v, want unrelated free-text query rejected", got)
	}
}

func TestLanguageMatchesNormalizesEquivalentLanguageForms(t *testing.T) {
	for _, pair := range [][2]string{
		{"en-US", "en"},
		{"eng", "en"},
		{"English", "eng"},
	} {
		if !languageMatches(pair[0], pair[1]) {
			t.Fatalf("languageMatches(%q, %q) = false, want true", pair[0], pair[1])
		}
	}
	if languageMatches("en", "fr") {
		t.Fatal("languageMatches(en, fr) = true, want false")
	}
}

func TestRankMatchesPreservesExactTitleCoreQuery(t *testing.T) {
	query := metadata.SearchQuery{Title: "Dune"}
	match := metadata.Match{Provider: "catalog", ProviderID: "dune", Title: "Dune", Authors: []string{"Frank Herbert"}}

	got := RankMatches(query, []metadata.Match{match})

	if ids := matchIDs(got); !reflect.DeepEqual(ids, []string{"dune"}) {
		t.Fatalf("RankMatches() IDs = %#v, want exact-title match", ids)
	}
}

func matchIDs(matches []metadata.Match) []string {
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		ids = append(ids, match.ProviderID)
	}
	return ids
}
