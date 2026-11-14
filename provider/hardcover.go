package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

const hardcoverBaseURL = "https://api.hardcover.app/v1/graphql"

type HardcoverClient struct {
	baseURL   string
	apiKey    string
	client    *http.Client
	userAgent string
}

func NewHardcoverClient(apiKey, userAgent string) *HardcoverClient {
	return NewHardcoverClientAt(hardcoverBaseURL, apiKey, userAgent)
}

func NewHardcoverClientAt(baseURL, apiKey, userAgent string) *HardcoverClient {
	return &HardcoverClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    normalizeHardcoverToken(apiKey),
		client:    http.DefaultClient,
		userAgent: userAgent,
	}
}

// normalizeHardcoverToken strips a leading "Bearer " from the configured key.
// Hardcover's settings page shows the token already prefixed with "Bearer ", so
// pasting it verbatim would otherwise produce a "Bearer Bearer ..." header that
// Hardcover rejects as "Malformed Authorization header".
func normalizeHardcoverToken(key string) string {
	k := strings.TrimSpace(key)
	if len(k) >= 7 && strings.EqualFold(k[:7], "bearer ") {
		k = strings.TrimSpace(k[7:])
	}
	return k
}

func (c *HardcoverClient) ID() string {
	return "hardcover"
}

func (c *HardcoverClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, nil
	}
	query := strings.TrimSpace(sourceQueryText(q))
	if query == "" {
		return nil, nil
	}
	// Hardcover's Hasura forbids _ilike filters ("ilike and related operations
	// are not permitted on this server"); its typesense-backed `search` root
	// field is the supported full-text path and returns a rich document we can
	// map without a follow-up fetch.
	const gql = `query SearchBooks($q: String!) {
  search(query: $q, query_type: "Book", per_page: 20) {
    results
  }
}`
	body, err := c.graphql(ctx, gql, map[string]any{"q": query})
	if err != nil {
		return nil, err
	}
	var resp hardcoverSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	matches := make([]metadata.Match, 0, len(resp.Data.Search.Results.Hits))
	for _, hit := range resp.Data.Search.Results.Hits {
		matches = append(matches, hit.Document.toMatch())
	}
	return matches, nil
}

func (c *HardcoverClient) Fetch(ctx context.Context, id string) (*metadata.Match, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, nil
	}
	if !asciiDigits(id) {
		return nil, nil
	}
	bookID, err := strconv.Atoi(id)
	if err != nil {
		return nil, nil
	}
	const gql = `query GetBook($id: Int!) {
  books_by_pk(id: $id) {
    id title description release_date pages
    contributions { author { name } }
    editions { isbn_13 isbn_10 }
    image { url }
  }
}`
	body, err := c.graphql(ctx, gql, map[string]any{"id": bookID})
	if err != nil {
		return nil, err
	}
	var resp hardcoverFetchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Data.Book == nil {
		return nil, nil
	}
	match := resp.Data.Book.toMatch()
	return &match, nil
}

func (c *HardcoverClient) graphql(ctx context.Context, query string, variables map[string]any) ([]byte, error) {
	payload, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	body, status, err := httpDoBytes(ctx, c.client, req)
	if status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Errors) > 0 {
		return nil, fmt.Errorf("hardcover graphql error: %s", envelope.Errors[0].Message)
	}
	return body, nil
}

func asciiDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r > unicode.MaxASCII || !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

type hardcoverFetchResponse struct {
	Data struct {
		Book *hardcoverBook `json:"books_by_pk"`
	} `json:"data"`
}

type hardcoverSearchResponse struct {
	Data struct {
		Search struct {
			Results struct {
				Hits []struct {
					Document hardcoverSearchDoc `json:"document"`
				} `json:"hits"`
			} `json:"results"`
		} `json:"search"`
	} `json:"data"`
}

// hardcoverSearchDoc is the typesense document returned by the `search` field.
// It carries enough metadata (cover, authors, isbns, series) to build a Match
// without a per-result fetch.
type hardcoverSearchDoc struct {
	ID                    json.Number     `json:"id"`
	Title                 string          `json:"title"`
	Subtitle              string          `json:"subtitle"`
	Description           string          `json:"description"`
	AuthorNames           []string        `json:"author_names"`
	Image                 *hardcoverImage `json:"image"`
	ISBNs                 []string        `json:"isbns"`
	ReleaseYear           int             `json:"release_year"`
	Pages                 int             `json:"pages"`
	Genres                []string        `json:"genres"`
	SeriesNames           []string        `json:"series_names"`
	FeaturedSeriesPosition json.Number    `json:"featured_series_position"`
}

func (d hardcoverSearchDoc) toMatch() metadata.Match {
	isbn := ""
	for _, candidate := range d.ISBNs {
		if normalized := metadata.NormalizeISBN(candidate); normalized != "" {
			isbn = normalized
			break
		}
	}
	coverURL := ""
	if d.Image != nil {
		coverURL = d.Image.URL
	}
	series := ""
	if len(d.SeriesNames) > 0 {
		series = d.SeriesNames[0]
	}
	return metadata.Match{
		Provider:       "hardcover",
		ProviderID:     d.ID.String(),
		Title:          d.Title,
		Subtitle:       d.Subtitle,
		Authors:        d.AuthorNames,
		Description:    d.Description,
		PublishYear:    d.ReleaseYear,
		ISBN:           isbn,
		Genres:         d.Genres,
		CoverURL:       coverURL,
		PageCount:      d.Pages,
		SeriesName:     series,
		SeriesPosition: d.FeaturedSeriesPosition.String(),
	}
}

type hardcoverBook struct {
	ID            int                     `json:"id"`
	Title         string                  `json:"title"`
	Description   string                  `json:"description"`
	ReleaseDate   string                  `json:"release_date"`
	Pages         int                     `json:"pages"`
	Contributions []hardcoverContribution `json:"contributions"`
	Editions      []hardcoverEdition      `json:"editions"`
	Image         *hardcoverImage         `json:"image"`
}

type hardcoverContribution struct {
	Author struct {
		Name string `json:"name"`
	} `json:"author"`
}

type hardcoverEdition struct {
	ISBN13 string `json:"isbn_13"`
	ISBN10 string `json:"isbn_10"`
}

type hardcoverImage struct {
	URL string `json:"url"`
}

func (b hardcoverBook) toMatch() metadata.Match {
	authors := make([]string, 0, len(b.Contributions))
	for _, contribution := range b.Contributions {
		if name := strings.TrimSpace(contribution.Author.Name); name != "" {
			authors = append(authors, name)
		}
	}
	isbn := ""
	for _, edition := range b.Editions {
		if edition.ISBN13 != "" {
			isbn = edition.ISBN13
			break
		}
	}
	if isbn == "" {
		for _, edition := range b.Editions {
			if edition.ISBN10 != "" {
				isbn = edition.ISBN10
				break
			}
		}
	}
	coverURL := ""
	if b.Image != nil {
		coverURL = b.Image.URL
	}
	return metadata.Match{
		Provider:    "hardcover",
		ProviderID:  strconv.Itoa(b.ID),
		Title:       b.Title,
		Authors:     authors,
		Description: b.Description,
		PublishYear: firstYear(b.ReleaseDate),
		ISBN:        isbn,
		CoverURL:    coverURL,
		PageCount:   b.Pages,
	}
}
