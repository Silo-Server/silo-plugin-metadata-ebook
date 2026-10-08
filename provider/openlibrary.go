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
)

const (
	openLibraryBaseURL   = "https://openlibrary.org"
	openLibraryCoversURL = "https://covers.openlibrary.org"
)

var openLibraryEditionIDRE = regexp.MustCompile(`^OL[0-9A-Za-z]+M$`)

type OpenLibraryClient struct {
	baseURL    string
	coversBase string
	client     *http.Client
	userAgent  string
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

	// search.json returns a minimal field set by default (no isbn, no
	// cover_edition_key), which leaves every doc without a usable provider ID.
	// Request the fields toMatch needs so search results are actually fetchable.
	const olSearchFields = "key,cover_edition_key,title,author_name,first_publish_year,isbn,language,subject,cover_i,number_of_pages_median,publisher"
	endpoint := fmt.Sprintf("%s/search.json?q=%s&fields=%s&limit=20",
		c.baseURL, url.QueryEscape(query), url.QueryEscape(olSearchFields))
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
	Key             string   `json:"key"`
	CoverEditionKey string   `json:"cover_edition_key"`
	Title           string   `json:"title"`
	AuthorName      []string `json:"author_name"`
	FirstPublish    int      `json:"first_publish_year"`
	ISBN            []string `json:"isbn"`
	Language        []string `json:"language"`
	Subject         []string `json:"subject"`
	CoverID         int      `json:"cover_i"`
	NumPages        int      `json:"number_of_pages_median"`
	Publisher       []string `json:"publisher"`
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
		// search.json keys are work keys (/works/OL…W), not editions, so prefer
		// the cover_edition_key (an OL…M edition ID) which Fetch can resolve.
		for _, key := range []string{d.CoverEditionKey, strings.TrimPrefix(d.Key, "/books/")} {
			if key = strings.TrimSpace(key); openLibraryEditionIDRE.MatchString(key) {
				providerID = key
				break
			}
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
