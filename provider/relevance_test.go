package provider

import (
	"testing"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

func TestRelevanceScore(t *testing.T) {
	// host folds author into the query string
	q := metadata.SearchQuery{Title: "Amanda in Holland Darlene Foster"}
	cases := []struct {
		name string
		m    metadata.Match
		min  float64
		max  float64
	}{
		{"exact title+author", metadata.Match{Title: "Amanda in Holland", Authors: []string{"Darlene Foster"}}, 0.99, 1.0},
		{"title only, no author", metadata.Match{Title: "Amanda in Holland"}, 0.49, 0.51}, // 2/4 query tokens
		{"unrelated magazine", metadata.Match{Title: "Moose Magazine March 2024"}, 0.0, 0.0},
		{"loose keyword overlap", metadata.Match{Title: "Amanda in Wonderland"}, 0.24, 0.26}, // amanda only -> 1/4
	}
	for _, tc := range cases {
		got := relevanceScore(q, tc.m)
		if got < tc.min || got > tc.max {
			t.Errorf("%s: relevanceScore=%.3f, want in [%.2f,%.2f]", tc.name, got, tc.min, tc.max)
		}
	}
	// empty query scores zero
	if s := relevanceScore(metadata.SearchQuery{}, metadata.Match{Title: "x y"}); s != 0 {
		t.Errorf("empty query: got %.3f want 0", s)
	}
}
