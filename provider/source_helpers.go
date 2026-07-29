package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-plugin-ebook-metadata/metadata"
)

func sourceQueryText(q metadata.SearchQuery) string {
	parts := make([]string, 0, 1+len(q.Authors))
	if title := strings.TrimSpace(q.Title); title != "" {
		parts = append(parts, title)
	}
	for _, author := range q.Authors {
		if author = strings.TrimSpace(author); author != "" {
			parts = append(parts, author)
		}
	}
	return strings.Join(parts, " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func firstYear(value string) int {
	for i := 0; i+4 <= len(value); i++ {
		year, err := strconv.Atoi(value[i : i+4])
		if err == nil && year > 0 {
			return year
		}
	}
	return 0
}

func single(value string) []string {
	if value = strings.TrimSpace(value); value == "" {
		return nil
	}
	return []string{value}
}

func httpDoBytes(ctx context.Context, client *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: request: %w", req.Method, redactURL(req.URL), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%s %s: read body: %w", req.Method, redactURL(req.URL), err)
	}
	if len(body) > maxResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf("%s %s: response body exceeds %d bytes", req.Method, redactURL(req.URL), maxResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return body, resp.StatusCode, newHTTPStatusError(req.Method, redactURL(req.URL), resp.StatusCode, resp.Header, time.Now())
	}
	return body, resp.StatusCode, nil
}

func redactURL(value *url.URL) string {
	if value == nil {
		return ""
	}
	clone := *value
	clone.Path = redactPathSegments(clone.EscapedPath())
	query := clone.Query()
	for key, values := range query {
		for i := range values {
			values[i] = "<redacted>"
		}
		query[key] = values
	}
	clone.RawQuery = query.Encode()
	return clone.String()
}

func redactPathSegments(path string) string {
	if path == "" {
		return ""
	}
	segments := strings.Split(path, "/")
	for i := 0; i+1 < len(segments); i++ {
		switch strings.ToLower(segments[i]) {
		case "dp", "md5":
			segments[i+1] = "redacted"
		}
	}
	return strings.Join(segments, "/")
}
