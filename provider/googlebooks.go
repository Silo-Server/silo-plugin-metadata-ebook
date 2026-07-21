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

const googleBooksBaseURL = "https://www.googleapis.com/books/v1"

var googleBooksVolumeIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{12}$`)

type GoogleBooksClient struct {
	baseURL   string
	apiKey    string
	client    *http.Client
	userAgent string
}

func NewGoogleBooksClient(apiKey, userAgent string) *GoogleBooksClient {
	return NewGoogleBooksClientAt(googleBooksBaseURL, apiKey, userAgent)
}

func NewGoogleBooksClientAt(baseURL, apiKey, userAgent string) *GoogleBooksClient {
	return &GoogleBooksClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		client:    http.DefaultClient,
		userAgent: userAgent,
	}
}

func (c *GoogleBooksClient) ID() string {
	return "googlebooks"
}

func (c *GoogleBooksClient) eligible() bool {
	return strings.TrimSpace(c.apiKey) != ""
}

func (c *GoogleBooksClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
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
		query = "isbn:" + isbn
	}
	values := url.Values{}
	values.Set("q", query)
	values.Set("maxResults", "20")
	if c.apiKey != "" {
		values.Set("key", c.apiKey)
	}
	body, err := httpGetBytes(ctx, c.client, c.baseURL+"/volumes?"+values.Encode(), c.userAgent)
	if err != nil {
		return nil, err
	}
	var resp googleBooksSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	matches := make([]metadata.Match, 0, len(resp.Items))
	for _, volume := range resp.Items {
		matches = append(matches, volume.toMatch())
	}
	return matches, nil
}

func (c *GoogleBooksClient) Fetch(ctx context.Context, id string) (*metadata.Match, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, nil
	}
	id = strings.TrimSpace(id)
	if isbn := metadata.NormalizeISBN(id); isbn != "" {
		matches, err := c.Search(ctx, metadata.SearchQuery{Title: isbn})
		if err != nil || len(matches) == 0 {
			return nil, err
		}
		return &matches[0], nil
	}
	if !googleBooksVolumeIDRE.MatchString(id) {
		return nil, nil
	}
	values := url.Values{}
	if c.apiKey != "" {
		values.Set("key", c.apiKey)
	}
	endpoint := fmt.Sprintf("%s/volumes/%s", c.baseURL, url.PathEscape(id))
	if encoded := values.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
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
	var volume googleBooksVolume
	if err := json.Unmarshal(body, &volume); err != nil {
		return nil, err
	}
	match := volume.toMatch()
	return &match, nil
}

type googleBooksSearchResponse struct {
	Items []googleBooksVolume `json:"items"`
}

type googleBooksVolume struct {
	ID         string                `json:"id"`
	VolumeInfo googleBooksVolumeInfo `json:"volumeInfo"`
}

type googleBooksVolumeInfo struct {
	Title               string                  `json:"title"`
	Authors             []string                `json:"authors"`
	Publisher           string                  `json:"publisher"`
	PublishedDate       string                  `json:"publishedDate"`
	Description         string                  `json:"description"`
	PageCount           int                     `json:"pageCount"`
	Categories          []string                `json:"categories"`
	Language            string                  `json:"language"`
	ImageLinks          *googleBooksImageLinks  `json:"imageLinks"`
	IndustryIdentifiers []googleBooksIdentifier `json:"industryIdentifiers"`
}

type googleBooksImageLinks struct {
	SmallThumbnail string `json:"smallThumbnail"`
	Thumbnail      string `json:"thumbnail"`
	Small          string `json:"small"`
	Medium         string `json:"medium"`
	Large          string `json:"large"`
	ExtraLarge     string `json:"extraLarge"`
}

type googleBooksIdentifier struct {
	Type       string `json:"type"`
	Identifier string `json:"identifier"`
}

func (v googleBooksVolume) toMatch() metadata.Match {
	info := v.VolumeInfo
	isbn := ""
	for _, identifier := range info.IndustryIdentifiers {
		if identifier.Type == "ISBN_13" {
			isbn = identifier.Identifier
			break
		}
	}
	if isbn == "" {
		for _, identifier := range info.IndustryIdentifiers {
			if identifier.Type == "ISBN_10" {
				isbn = identifier.Identifier
				break
			}
		}
	}
	return metadata.Match{
		Provider:    "googlebooks",
		ProviderID:  v.ID,
		Title:       info.Title,
		Authors:     info.Authors,
		Description: info.Description,
		Publisher:   info.Publisher,
		PublishYear: firstYear(info.PublishedDate),
		ISBN:        isbn,
		Genres:      info.Categories,
		CoverURL:    googleBooksCoverURL(info.ImageLinks),
		Language:    info.Language,
		PageCount:   info.PageCount,
	}
}

func googleBooksCoverURL(links *googleBooksImageLinks) string {
	if links == nil {
		return ""
	}
	for _, value := range []string{
		links.ExtraLarge,
		links.Large,
		links.Medium,
		links.Small,
		links.Thumbnail,
		links.SmallThumbnail,
	} {
		if value != "" {
			return strings.Replace(value, "http://", "https://", 1)
		}
	}
	return ""
}
