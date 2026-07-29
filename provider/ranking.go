package provider

import (
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

const (
	matchConfidenceTitle        = 100
	matchConfidenceTitleContext = 200
	matchConfidenceTitleAuthor  = 300
	matchConfidenceISBN         = 400
)

type rankedMatch struct {
	match metadata.Match
	score int
}

func RankMatches(query metadata.SearchQuery, matches []metadata.Match) []metadata.Match {
	ranked := make([]rankedMatch, 0, len(matches))
	for _, match := range matches {
		if score := matchScore(query, match); score > 0 {
			ranked = append(ranked, rankedMatch{match: match, score: score})
		}
	}

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return matchSortKey(ranked[i].match) < matchSortKey(ranked[j].match)
	})

	deduplicated := make([]rankedMatch, 0, len(ranked))
	seen := make(map[string]bool, len(ranked))
	for _, candidate := range ranked {
		key := matchDedupKey(candidate.match)
		if seen[key] {
			continue
		}
		seen[key] = true
		deduplicated = append(deduplicated, candidate)
	}

	titleOnlyCounts := make(map[string]int)
	for _, candidate := range deduplicated {
		if candidate.score == matchConfidenceTitle {
			titleOnlyCounts[normalizeMatchText(candidate.match.Title)]++
		}
	}

	out := make([]metadata.Match, 0, len(deduplicated))
	for _, candidate := range deduplicated {
		if candidate.score == matchConfidenceTitle &&
			titleOnlyCounts[normalizeMatchText(candidate.match.Title)] > 1 {
			continue
		}
		out = append(out, candidate.match)
	}
	return out
}

func matchScore(query metadata.SearchQuery, match metadata.Match) int {
	if isbn := queryISBN(query); isbn != "" && metadata.EquivalentISBN(match.ISBN, isbn) {
		return matchConfidenceISBN
	}

	titleMatches, freeTextAuthorMatches := titleEvidence(query, match)
	if !titleMatches || hasContradictoryEvidence(query, match) {
		return 0
	}
	if freeTextAuthorMatches || authorsOverlap(query.Authors, match.Authors) {
		return matchConfidenceTitleAuthor
	}

	contextScore := 0
	if query.Year > 0 && query.Year == match.PublishYear {
		contextScore++
	}
	if languageMatches(query.Language, match.Language) {
		contextScore++
	}
	if contextScore > 0 {
		return matchConfidenceTitleContext + contextScore
	}
	return matchConfidenceTitle
}

func titleEvidence(query metadata.SearchQuery, match metadata.Match) (bool, bool) {
	queryTitle := normalizeMatchText(query.Title)
	matchTitle := normalizeMatchText(match.Title)
	if queryTitle == "" || matchTitle == "" {
		return false, false
	}
	if queryTitle == matchTitle {
		return true, false
	}
	if len(query.Authors) != 0 {
		return false, false
	}

	queryTokens := strings.Fields(queryTitle)
	titleTokens := strings.Fields(matchTitle)
	authorTokens := normalizedTokens(match.Authors)
	if len(queryTokens) == 0 || len(titleTokens) == 0 {
		return false, false
	}

	titleSet := tokenSet(titleTokens)
	authorSet := tokenSet(authorTokens)
	var titleQueryTokens []string
	authorMatches := false
	for _, token := range queryTokens {
		_, inTitle := titleSet[token]
		_, inAuthor := authorSet[token]
		if !inTitle && !inAuthor {
			return false, false
		}
		if inTitle {
			titleQueryTokens = append(titleQueryTokens, token)
		}
		if inAuthor && !inTitle {
			authorMatches = true
		}
	}

	fullTitle := allTokensPresent(titleTokens, tokenSet(queryTokens))
	partialTitle := len(titleQueryTokens) >= 2 && containsTokenSequence(titleTokens, titleQueryTokens)
	if authorMatches && (fullTitle || partialTitle) {
		return true, true
	}
	if !authorMatches && len(titleQueryTokens) == len(queryTokens) && partialTitle {
		return true, false
	}
	return false, false
}

func hasContradictoryEvidence(query metadata.SearchQuery, match metadata.Match) bool {
	if len(query.Authors) > 0 && len(match.Authors) > 0 && !authorsOverlap(query.Authors, match.Authors) {
		return true
	}
	if query.Year > 0 && match.PublishYear > 0 && query.Year != match.PublishYear {
		return true
	}
	return strings.TrimSpace(query.Language) != "" &&
		strings.TrimSpace(match.Language) != "" &&
		!languageMatches(query.Language, match.Language)
}

func hasHighConfidenceMatch(query metadata.SearchQuery, matches []metadata.Match) bool {
	for _, match := range matches {
		if matchScore(query, match) >= matchConfidenceTitleContext {
			return true
		}
	}
	return false
}

func authorsOverlap(left, right []string) bool {
	for _, a := range left {
		for _, b := range right {
			if normalizeMatchText(a) != "" && normalizeMatchText(a) == normalizeMatchText(b) {
				return true
			}
		}
	}
	return false
}

func languageMatches(left, right string) bool {
	left = canonicalLanguage(left)
	right = canonicalLanguage(right)
	return left != "" && left == right
}

func canonicalLanguage(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	if index := strings.IndexByte(value, '-'); index >= 0 {
		value = value[:index]
	}
	switch value {
	case "english", "eng":
		return "en"
	case "french", "fra", "fre":
		return "fr"
	case "german", "deu", "ger":
		return "de"
	case "spanish", "spa":
		return "es"
	case "italian", "ita":
		return "it"
	case "portuguese", "por":
		return "pt"
	case "dutch", "nld", "dut":
		return "nl"
	case "chinese", "zho", "chi":
		return "zh"
	case "japanese", "jpn":
		return "ja"
	default:
		return value
	}
}

func normalizedTokens(values []string) []string {
	var tokens []string
	for _, value := range values {
		tokens = append(tokens, strings.Fields(normalizeMatchText(value))...)
	}
	return tokens
}

func tokenSet(tokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		set[token] = struct{}{}
	}
	return set
}

func allTokensPresent(tokens []string, available map[string]struct{}) bool {
	for _, token := range tokens {
		if _, ok := available[token]; !ok {
			return false
		}
	}
	return true
}

func containsTokenSequence(tokens, sequence []string) bool {
	if len(sequence) == 0 || len(sequence) > len(tokens) {
		return false
	}
	for start := 0; start+len(sequence) <= len(tokens); start++ {
		matches := true
		for offset := range sequence {
			if tokens[start+offset] != sequence[offset] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func matchDedupKey(match metadata.Match) string {
	if isbn := metadata.CanonicalISBN(match.ISBN); isbn != "" {
		return "isbn:" + isbn
	}
	provider := normalizeMatchText(match.Provider)
	providerID := strings.TrimSpace(match.ProviderID)
	if provider != "" && providerID != "" {
		return "provider:" + provider + ":" + providerID
	}
	return "metadata:" + matchSortKey(match)
}

func matchSortKey(match metadata.Match) string {
	authors := make([]string, 0, len(match.Authors))
	for _, author := range match.Authors {
		authors = append(authors, normalizeMatchText(author))
	}
	return strings.Join([]string{
		normalizeMatchText(match.Provider),
		strings.TrimSpace(match.ProviderID),
		metadata.CanonicalISBN(match.ISBN),
		normalizeMatchText(match.Title),
		strings.Join(authors, "\x00"),
		strconv.Itoa(match.PublishYear),
		strings.ToLower(strings.TrimSpace(match.Language)),
	}, "\x00")
}

func normalizeMatchText(value string) string {
	var normalized strings.Builder
	needsSpace := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			if needsSpace && normalized.Len() > 0 {
				normalized.WriteByte(' ')
			}
			normalized.WriteRune(r)
			needsSpace = false
		default:
			needsSpace = true
		}
	}
	return normalized.String()
}
