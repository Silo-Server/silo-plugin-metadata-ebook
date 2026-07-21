package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
	"golang.org/x/time/rate"
)

const (
	openLibraryBaseURL   = "https://openlibrary.org"
	openLibraryCoversURL = "https://covers.openlibrary.org"
	openLibraryRPM       = 60
)

var openLibraryEditionIDRE = regexp.MustCompile(`^OL[0-9A-Za-z]+M$`)

type OpenLibraryClient struct {
	baseURL    string
	coversBase string
	client     *http.Client
	userAgent  string
	limiter    *rate.Limiter
}

func NewOpenLibraryClient(userAgent string) *OpenLibraryClient {
	return NewOpenLibraryClientAt(openLibraryBaseURL, openLibraryCoversURL, userAgent)
}

func NewOpenLibraryClientAt(baseURL, coversURL, userAgent string) *OpenLibraryClient {
	return &OpenLibraryClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		coversBase: strings.TrimRight(coversURL, "/"),
		client:     http.DefaultClient,
		userAgent:  userAgent,
		limiter:    newLimiter(openLibraryRPM),
	}
}

func (c *OpenLibraryClient) ID() string {
	return "openlibrary"
}

func (c *OpenLibraryClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	query := strings.TrimSpace(sourceQueryText(q))
	if query == "" {
		query = strings.TrimSpace(q.ProviderIDs["isbn"])
	}
	if query == "" {
		return nil, nil
	}
	if isbn := metadata.NormalizeISBN(query); isbn != "" {
		match, err := c.Fetch(ctx, isbn)
		if match == nil || err != nil {
			return nil, err
		}
		return []metadata.Match{*match}, nil
	}
	if err := waitForLimiter(ctx, c.limiter); err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/search.json?q=%s&limit=20", c.baseURL, url.QueryEscape(query))
	body, err := httpGetBytes(ctx, c.client, endpoint, c.userAgent)
	if err != nil {
		return nil, err
	}
	var resp openLibrarySearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	matches := make([]metadata.Match, 0, len(resp.Docs))
	for _, doc := range resp.Docs {
		match := doc.toMatch(c.coversBase)
		if match.ProviderID != "" {
			matches = append(matches, match)
		}
	}
	return matches, nil
}

func (c *OpenLibraryClient) Fetch(ctx context.Context, id string) (*metadata.Match, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	var path string
	if isbn := metadata.NormalizeISBN(id); isbn != "" {
		path = "/isbn/" + url.PathEscape(isbn) + ".json"
	} else if openLibraryEditionIDRE.MatchString(id) {
		path = "/books/" + url.PathEscape(id) + ".json"
	} else {
		return nil, nil
	}
	if err := waitForLimiter(ctx, c.limiter); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
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
	var edition openLibraryEdition
	if err := json.Unmarshal(body, &edition); err != nil {
		return nil, err
	}
	match := edition.toMatch(c.coversBase)
	return &match, nil
}

type openLibraryEdition struct {
	Key         string               `json:"key"`
	Title       string               `json:"title"`
	Subtitle    string               `json:"subtitle"`
	Authors     []string             `json:"author_name"`
	Description any                  `json:"description"`
	Publishers  []string             `json:"publishers"`
	PublishDate string               `json:"publish_date"`
	Languages   []openLibraryLangRef `json:"languages"`
	Subjects    []string             `json:"subjects"`
	ISBN10      []string             `json:"isbn_10"`
	ISBN13      []string             `json:"isbn_13"`
	NumPages    int                  `json:"number_of_pages"`
	Covers      []int                `json:"covers"`
}

type openLibraryLangRef struct {
	Key string `json:"key"`
}

type openLibrarySearchResponse struct {
	Docs []openLibrarySearchDoc `json:"docs"`
}

type openLibrarySearchDoc struct {
	Key          string   `json:"key"`
	Title        string   `json:"title"`
	AuthorName   []string `json:"author_name"`
	FirstPublish int      `json:"first_publish_year"`
	ISBN         []string `json:"isbn"`
	Language     []string `json:"language"`
	Subject      []string `json:"subject"`
	CoverID      int      `json:"cover_i"`
	NumPages     int      `json:"number_of_pages_median"`
	Publisher    []string `json:"publisher"`
}

func (e openLibraryEdition) toMatch(coversBase string) metadata.Match {
	description := ""
	switch d := e.Description.(type) {
	case string:
		description = d
	case map[string]any:
		description, _ = d["value"].(string)
	}
	language := ""
	if len(e.Languages) > 0 {
		language = strings.TrimPrefix(e.Languages[0].Key, "/languages/")
	}
	coverURL := ""
	if len(e.Covers) > 0 && coversBase != "" {
		coverURL = fmt.Sprintf("%s/b/id/%d-L.jpg", coversBase, e.Covers[0])
	}
	return metadata.Match{
		Provider:    "openlibrary",
		ProviderID:  strings.TrimPrefix(e.Key, "/books/"),
		Title:       e.Title,
		Subtitle:    e.Subtitle,
		Authors:     e.Authors,
		Description: description,
		Publisher:   firstNonEmpty(e.Publishers...),
		PublishYear: firstYear(e.PublishDate),
		ISBN:        openLibraryISBN(e.ISBN13, e.ISBN10),
		Genres:      e.Subjects,
		CoverURL:    coverURL,
		Language:    language,
		PageCount:   e.NumPages,
	}
}

func openLibraryISBN(isbn13, isbn10 []string) string {
	if isbn := firstNonEmpty(isbn13...); isbn != "" {
		return isbn
	}
	return firstNonEmpty(isbn10...)
}

func (d openLibrarySearchDoc) toMatch(coversBase string) metadata.Match {
	coverURL := ""
	if d.CoverID > 0 && coversBase != "" {
		coverURL = fmt.Sprintf("%s/b/id/%d-L.jpg", coversBase, d.CoverID)
	}
	isbn := metadata.NormalizeISBN(firstNonEmpty(d.ISBN...))
	providerID := isbn
	if providerID == "" {
		key := strings.TrimPrefix(d.Key, "/books/")
		if openLibraryEditionIDRE.MatchString(key) {
			providerID = key
		}
	}
	return metadata.Match{
		Provider:    "openlibrary",
		ProviderID:  providerID,
		Title:       d.Title,
		Authors:     d.AuthorName,
		Publisher:   firstNonEmpty(d.Publisher...),
		PublishYear: d.FirstPublish,
		ISBN:        isbn,
		Genres:      d.Subject,
		CoverURL:    coverURL,
		Language:    firstNonEmpty(d.Language...),
		PageCount:   d.NumPages,
	}
}

// SetRequestsPerMinute replaces the client's rate limit. Used to tune the
// OpenLibrary request budget (and by tests to force saturation semantics).
func (c *OpenLibraryClient) SetRequestsPerMinute(rpm float64) {
	c.limiter = newLimiter(rpm)
}
