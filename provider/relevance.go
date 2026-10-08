package provider

import (
	"strings"
	"unicode"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

// relevanceStopwords are dropped before scoring so common filler words don't
// inflate coverage between unrelated titles.
var relevanceStopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "of": {}, "and": {}, "or": {}, "to": {},
	"in": {}, "on": {}, "for": {}, "with": {}, "at": {}, "by": {}, "is": {},
}

// relevanceScore measures how much of the query is covered by a match. The host
// folds the author into the query string, so the query tokens span title +
// author; a match is scored against its title, subtitle, and author names. The
// score is the fraction of query tokens present in the match (coverage), which
// rewards matches that account for the whole query and lets truncated stored
// titles still match a fuller catalog title.
func relevanceScore(q metadata.SearchQuery, m metadata.Match) float64 {
	qTokens := tokenizeRelevance(q.Title)
	for _, a := range q.Authors {
		qTokens = append(qTokens, tokenizeRelevance(a)...)
	}
	qset := tokenSet(qTokens)
	if len(qset) == 0 {
		return 0
	}

	mTokens := tokenizeRelevance(m.Title)
	mTokens = append(mTokens, tokenizeRelevance(m.Subtitle)...)
	for _, a := range m.Authors {
		mTokens = append(mTokens, tokenizeRelevance(a)...)
	}
	mset := tokenSet(mTokens)
	if len(mset) == 0 {
		return 0
	}

	covered := 0
	for token := range qset {
		if _, ok := mset[token]; ok {
			covered++
		}
	}
	return float64(covered) / float64(len(qset))
}

func tokenizeRelevance(value string) []string {
	fields := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		if _, stop := relevanceStopwords[f]; stop {
			continue
		}
		out = append(out, f)
	}
	return out
}

func tokenSet(tokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		set[t] = struct{}{}
	}
	return set
}
