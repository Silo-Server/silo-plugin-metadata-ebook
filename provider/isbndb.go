package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

const isbndbBaseURL = "https://api2.isbndb.com"

type ISBNdbClient struct {
	baseURL   string
	apiKey    string
	client    *http.Client
	userAgent string
}

func NewISBNdbClient(apiKey, userAgent string) *ISBNdbClient {
	return NewISBNdbClientAt(isbndbBaseURL, apiKey, userAgent)
}

func NewISBNdbClientAt(baseURL, apiKey, userAgent string) *ISBNdbClient {
	return &ISBNdbClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		client:    http.DefaultClient,
		userAgent: userAgent,
	}
}

func (c *ISBNdbClient) ID() string {
	return "isbndb"
}

func (c *ISBNdbClient) eligible() bool {
	return strings.TrimSpace(c.apiKey) != ""
}

func (c *ISBNdbClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, nil
	}
	query := strings.TrimSpace(sourceQueryText(q))
	if query == "" {
		query = strings.TrimSpace(q.ProviderIDs["isbn"])
	}
	if query == "" {
		return nil, nil
	}
	if isbn := metadata.NormalizeISBN(query); isbn != "" {
		query = isbn
	}
	endpoint := fmt.Sprintf("%s/books/%s?pageSize=20", c.baseURL, url.PathEscape(query))
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.addHeaders(req)
	body, status, err := httpDoBytes(ctx, c.client, req)
	if status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var resp isbndbSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	matches := make([]metadata.Match, 0, len(resp.Books))
	for _, book := range resp.Books {
		matches = append(matches, book.toMatch())
	}
	return matches, nil
}

func (c *ISBNdbClient) Fetch(ctx context.Context, id string) (*metadata.Match, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, nil
	}
	isbn := metadata.NormalizeISBN(id)
	if isbn == "" {
		return nil, nil
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/book/"+url.PathEscape(isbn), nil)
	if err != nil {
		return nil, err
	}
	c.addHeaders(req)
	body, status, err := httpDoBytes(ctx, c.client, req)
	if status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var resp isbndbBookResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Book == nil {
		return nil, nil
	}
	match := resp.Book.toMatch()
	return &match, nil
}

func (c *ISBNdbClient) addHeaders(req *http.Request) {
	req.Header.Set("Authorization", c.apiKey)
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
}

type isbndbBookResponse struct {
	Book *isbndbBook `json:"book"`
}

type isbndbSearchResponse struct {
	Books []isbndbBook `json:"books"`
}

type isbndbBook struct {
	Title         string   `json:"title"`
	ISBN          string   `json:"isbn"`
	ISBN13        string   `json:"isbn13"`
	Publisher     string   `json:"publisher"`
	Language      string   `json:"language"`
	DatePublished string   `json:"date_published"`
	Synopsis      string   `json:"synopsis"`
	Overview      string   `json:"overview"`
	Image         string   `json:"image"`
	Authors       []string `json:"authors"`
	Subjects      []string `json:"subjects"`
	Pages         int      `json:"pages"`
}

func (b isbndbBook) toMatch() metadata.Match {
	isbn := firstNonEmpty(b.ISBN13, b.ISBN)
	return metadata.Match{
		Provider:    "isbndb",
		ProviderID:  isbn,
		Title:       b.Title,
		Authors:     b.Authors,
		Description: firstNonEmpty(b.Synopsis, b.Overview),
		Publisher:   b.Publisher,
		PublishYear: firstYear(b.DatePublished),
		ISBN:        isbn,
		Genres:      b.Subjects,
		CoverURL:    b.Image,
		Language:    b.Language,
		PageCount:   b.Pages,
	}
}
