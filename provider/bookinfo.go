package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

const (
	bookInfoID      = "bookinfo"
	bookInfoBaseURL = "https://api.bookinfo.pro"
	// maxBookInfoWorks bounds the per-search work fetches. /search returns only
	// IDs, so each candidate needs a /work call; the endpoint ranks by Goodreads
	// relevance, so the top few cover the right book without an N+1 storm.
	maxBookInfoWorks = 3
)

// BookInfoClient is a read-only client for the public reading-glasses instance
// at api.bookinfo.pro, which serves Goodreads work/edition/series data. It fills
// the self-published and foreign-language tail that OpenLibrary/Google Books
// miss, and carries Goodreads cover images on each match.
type BookInfoClient struct {
	baseURL   string
	client    *http.Client
	userAgent string
}

func NewBookInfoClient(userAgent string) *BookInfoClient {
	return NewBookInfoClientAt(bookInfoBaseURL, userAgent)
}

func NewBookInfoClientAt(baseURL, userAgent string) *BookInfoClient {
	return &BookInfoClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		client:    http.DefaultClient,
		userAgent: userAgent,
	}
}

func (c *BookInfoClient) ID() string { return bookInfoID }

func (c *BookInfoClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	query := strings.TrimSpace(sourceQueryText(q))
	if query == "" {
		return nil, nil
	}
	endpoint := fmt.Sprintf("%s/search?q=%s", c.baseURL, url.QueryEscape(query))
	body, status, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, nil
	}
	var hits []bookInfoSearchHit
	if err := json.Unmarshal(body, &hits); err != nil {
		return nil, err
	}
	matches := make([]metadata.Match, 0, maxBookInfoWorks)
	seen := make(map[int]bool)
	for _, hit := range hits {
		if len(matches) >= maxBookInfoWorks {
			break
		}
		if hit.WorkID == 0 || seen[hit.WorkID] {
			continue
		}
		seen[hit.WorkID] = true
		match, err := c.fetchWork(ctx, hit.WorkID)
		if err != nil || match == nil {
			continue
		}
		matches = append(matches, *match)
	}
	return matches, nil
}

func (c *BookInfoClient) Fetch(ctx context.Context, id string) (*metadata.Match, error) {
	workID, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil {
		return nil, nil
	}
	return c.fetchWork(ctx, workID)
}

func (c *BookInfoClient) fetchWork(ctx context.Context, workID int) (*metadata.Match, error) {
	endpoint := fmt.Sprintf("%s/work/%d", c.baseURL, workID)
	body, status, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, nil
	}
	var work bookInfoWork
	if err := json.Unmarshal(body, &work); err != nil {
		return nil, err
	}
	match := work.toMatch()
	if match.Title == "" {
		return nil, nil
	}
	return &match, nil
}

func (c *BookInfoClient) get(ctx context.Context, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	return httpDoBytes(ctx, c.client, req)
}

type bookInfoSearchHit struct {
	BookID int `json:"bookId"`
	WorkID int `json:"workId"`
}

type bookInfoWork struct {
	ForeignID   int                `json:"ForeignId"`
	Title       string             `json:"Title"`
	ReleaseDate string             `json:"ReleaseDate"`
	Genres      []string           `json:"Genres"`
	BestBookID  int                `json:"BestBookId"`
	Books       []bookInfoEdition  `json:"Books"`
	Series      []bookInfoSeries   `json:"Series"`
	Authors     []bookInfoAuthor   `json:"Authors"`
}

type bookInfoAuthor struct {
	Name string `json:"Name"`
}

type bookInfoSeries struct {
	Title     string `json:"Title"`
	LinkItems []struct {
		ForeignWorkID   int    `json:"ForeignWorkId"`
		PositionInSeries string `json:"PositionInSeries"`
	} `json:"LinkItems"`
}

type bookInfoEdition struct {
	ForeignID   int    `json:"ForeignId"`
	Asin        string `json:"Asin"`
	Description string `json:"Description"`
	Title       string `json:"Title"`
	Language    string `json:"Language"`
	Publisher   string `json:"Publisher"`
	ImageURL    string `json:"ImageUrl"`
	IsEbook     bool   `json:"IsEbook"`
	NumPages    int    `json:"NumPages"`
	ReleaseDate string `json:"ReleaseDate"`
}

// pickEdition prefers the edition most likely to carry a usable cover: the
// work's designated best book, then any ebook with a cover, then any edition
// with a cover, falling back to the best book or the first edition.
func (w bookInfoWork) pickEdition() *bookInfoEdition {
	var withImage, ebookWithImage, best *bookInfoEdition
	for i := range w.Books {
		ed := &w.Books[i]
		if ed.ForeignID == w.BestBookID {
			best = ed
			if ed.ImageURL != "" {
				return ed
			}
		}
		if ed.ImageURL != "" {
			if withImage == nil {
				withImage = ed
			}
			if ed.IsEbook && ebookWithImage == nil {
				ebookWithImage = ed
			}
		}
	}
	switch {
	case ebookWithImage != nil:
		return ebookWithImage
	case withImage != nil:
		return withImage
	case best != nil:
		return best
	case len(w.Books) > 0:
		return &w.Books[0]
	default:
		return nil
	}
}

func (w bookInfoWork) toMatch() metadata.Match {
	authors := make([]string, 0, len(w.Authors))
	for _, a := range w.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			authors = append(authors, name)
		}
	}
	match := metadata.Match{
		Provider:    bookInfoID,
		ProviderID:  strconv.Itoa(w.ForeignID),
		Title:       w.Title,
		Authors:     authors,
		Genres:      w.Genres,
		PublishYear: firstYear(w.ReleaseDate),
	}
	if ed := w.pickEdition(); ed != nil {
		match.Description = ed.Description
		match.Publisher = ed.Publisher
		match.Language = ed.Language
		match.PageCount = ed.NumPages
		match.CoverURL = ed.ImageURL
		if match.Title == "" {
			match.Title = ed.Title
		}
		if match.PublishYear == 0 {
			match.PublishYear = firstYear(ed.ReleaseDate)
		}
	}
	if len(w.Series) > 0 {
		match.SeriesName = w.Series[0].Title
		for _, link := range w.Series[0].LinkItems {
			if link.ForeignWorkID == w.ForeignID && strings.TrimSpace(link.PositionInSeries) != "" {
				match.SeriesPosition = strings.TrimSpace(link.PositionInSeries)
				break
			}
		}
	}
	return match
}
