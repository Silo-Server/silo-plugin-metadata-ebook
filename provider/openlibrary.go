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

	// openLibrarySearchFields is what search.json must return. Each doc is a
	// work, and the service sends no edition data unless asked: "editions" adds
	// the one edition that best matches the query, the only edition ID a doc
	// carries. "key" has to stay in the list even though the work key is not
	// read: without it the service returns an empty editions list for every
	// doc.
	openLibrarySearchFields = "key,title,author_name,first_publish_year,cover_i,editions,editions.key"

	// openLibraryRequestsPerMinute is OpenLibrary's limit of one request a
	// second.
	openLibraryRequestsPerMinute = 60
)

var openLibraryEditionIDRE = regexp.MustCompile(`^OL[0-9A-Za-z]+M$`)

// openLibraryHTTPClient is shared by every client NewOpenLibraryClient builds,
// so reconfiguring the plugin does not hand OpenLibrary a second request
// budget.
var openLibraryHTTPClient = &http.Client{
	Transport: rateLimitedTransport{limiter: newLimiter(openLibraryRequestsPerMinute)},
}

type OpenLibraryClient struct {
	baseURL    string
	coversBase string
	client     *http.Client
	userAgent  string
}

func NewOpenLibraryClient(userAgent string) *OpenLibraryClient {
	client := NewOpenLibraryClientAt(openLibraryBaseURL, openLibraryCoversURL, userAgent)
	client.client = openLibraryHTTPClient
	return client
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
	// An item that already has an ISBN is looked up by it and never by its
	// text: a text search returns whichever edition matches the words best,
	// and that edition's ISBN would contradict the item's own.
	query := metadata.NormalizeISBN(q.ProviderIDs["isbn"])
	if query == "" {
		query = strings.TrimSpace(sourceQueryText(q))
	}
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

	endpoint := fmt.Sprintf("%s/search.json?q=%s&limit=20&fields=%s", c.baseURL, url.QueryEscape(query), openLibrarySearchFields)
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
	Title        string   `json:"title"`
	AuthorName   []string `json:"author_name"`
	FirstPublish int      `json:"first_publish_year"`
	CoverID      int      `json:"cover_i"`
	Editions     struct {
		Docs []struct {
			Key string `json:"key"`
		} `json:"docs"`
	} `json:"editions"`
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

// toMatch maps a work-level search doc. The provider ID is the matched
// edition, because Fetch resolves only edition IDs. A doc that names no edition
// gets no provider ID, and Search drops it: its work key cannot be fetched.
//
// The match carries no ISBN. A work's "isbn" field lists every edition's
// ISBNs, and the matched edition's own list also holds an ISBN-13 that the
// search index derived from an ISBN-10, which the edition record may not have.
// Fetch reads the ISBN from the edition record instead.
func (d openLibrarySearchDoc) toMatch(coversBase string) metadata.Match {
	coverURL := ""
	if d.CoverID > 0 && coversBase != "" {
		coverURL = fmt.Sprintf("%s/b/id/%d-L.jpg", coversBase, d.CoverID)
	}
	return metadata.Match{
		Provider:    "openlibrary",
		ProviderID:  d.editionID(),
		Title:       d.Title,
		Authors:     d.AuthorName,
		PublishYear: d.FirstPublish,
		CoverURL:    coverURL,
	}
}

func (d openLibrarySearchDoc) editionID() string {
	for _, edition := range d.Editions.Docs {
		id := strings.TrimPrefix(edition.Key, "/books/")
		if openLibraryEditionIDRE.MatchString(id) {
			return id
		}
	}
	return ""
}
