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
		apiKey:    apiKey,
		client:    http.DefaultClient,
		userAgent: userAgent,
	}
}

func (c *HardcoverClient) ID() string {
	return "hardcover"
}

func (c *HardcoverClient) eligible() bool {
	return strings.TrimSpace(c.apiKey) != ""
}

func (c *HardcoverClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, nil
	}
	query := strings.TrimSpace(sourceQueryText(q))
	if query == "" {
		return nil, nil
	}
	// Hardcover forbids _ilike (and friends) on its Hasura layer — arbitrary
	// where-filters answer 403. Text search goes through the dedicated
	// Typesense-backed `search` field instead, which returns raw Typesense
	// results ({found, hits: [{document: {...}}]}).
	const gql = `query SearchBooks($q: String!) {
  search(query: $q, query_type: "Book", per_page: 20, page: 1) {
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
	results, err := resp.Data.Search.typesenseResults()
	if err != nil {
		return nil, err
	}
	matches := make([]metadata.Match, 0, len(results.Hits))
	for _, hit := range results.Hits {
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
		Search hardcoverSearchPayload `json:"search"`
	} `json:"data"`
}

// hardcoverSearchPayload carries the `search.results` jsonb, which Hasura may
// serialize as an object or a JSON-encoded string depending on version.
type hardcoverSearchPayload struct {
	Results json.RawMessage `json:"results"`
}

type hardcoverTypesenseResults struct {
	Found int `json:"found"`
	Hits  []struct {
		Document hardcoverSearchDocument `json:"document"`
	} `json:"hits"`
}

func (p hardcoverSearchPayload) typesenseResults() (*hardcoverTypesenseResults, error) {
	raw := bytes.TrimSpace(p.Results)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return &hardcoverTypesenseResults{}, nil
	}
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, fmt.Errorf("hardcover search results string: %w", err)
		}
		raw = []byte(encoded)
	}
	var results hardcoverTypesenseResults
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("hardcover search results: %w", err)
	}
	return &results, nil
}

// hardcoverSearchDocument is the Typesense book document shape.
type hardcoverSearchDocument struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	AuthorNames []string        `json:"author_names"`
	ISBNs       []string        `json:"isbns"`
	ReleaseYear int             `json:"release_year"`
	Pages       int             `json:"pages"`
	Image       *hardcoverImage `json:"image"`
}

func (d hardcoverSearchDocument) toMatch() metadata.Match {
	authors := make([]string, 0, len(d.AuthorNames))
	for _, name := range d.AuthorNames {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			authors = append(authors, trimmed)
		}
	}
	// Prefer a 13-digit bookland ISBN; the isbns array mixes 10/13-digit
	// forms across every edition and language.
	isbn := ""
	for _, candidate := range d.ISBNs {
		if len(candidate) == 13 && (strings.HasPrefix(candidate, "978") || strings.HasPrefix(candidate, "979")) {
			isbn = candidate
			break
		}
	}
	if isbn == "" {
		for _, candidate := range d.ISBNs {
			if len(candidate) == 10 {
				isbn = candidate
				break
			}
		}
	}
	coverURL := ""
	if d.Image != nil {
		coverURL = d.Image.URL
	}
	return metadata.Match{
		Provider:    "hardcover",
		ProviderID:  d.ID,
		Title:       d.Title,
		Authors:     authors,
		Description: d.Description,
		PublishYear: d.ReleaseYear,
		ISBN:        isbn,
		CoverURL:    coverURL,
		PageCount:   d.Pages,
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
