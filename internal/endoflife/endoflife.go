// Package endoflife reads release cycles from the endoflife.date v1 API.
package endoflife

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dmleet/version-monitor/internal/model"
)

// DefaultBaseURL is the endoflife.date v1 API root.
const DefaultBaseURL = "https://endoflife.date/api/v1"

// Client queries endoflife.date.
type Client struct {
	HTTP      *http.Client
	BaseURL   string // default DefaultBaseURL
	UserAgent string
}

type response struct {
	Result struct {
		Name     string `json:"name"`
		Releases []struct {
			Name    string  `json:"name"`
			IsEOL   bool    `json:"isEol"`
			EOLFrom *string `json:"eolFrom"`
			Latest  *struct {
				Name string `json:"name"`
			} `json:"latest"`
		} `json:"releases"`
	} `json:"result"`
}

// Product returns the product's cycles in API order (newest first).
// A 404 returns an error that includes the product name and says it is unknown.
func (c *Client) Product(ctx context.Context, name string) ([]model.Cycle, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u := strings.TrimRight(base, "/") + "/products/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("endoflife: build request for %s: %w", u, err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "version-monitor"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("endoflife: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("endoflife: unknown product %q: GET %s: HTTP status %d", name, u, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("endoflife: GET %s: unexpected HTTP status %d", u, resp.StatusCode)
	}
	var r response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("endoflife: decode %s (HTTP %d): %w", u, resp.StatusCode, err)
	}
	cycles := make([]model.Cycle, 0, len(r.Result.Releases))
	for _, rel := range r.Result.Releases {
		cy := model.Cycle{Name: rel.Name, IsEOL: rel.IsEOL}
		if rel.Latest != nil {
			cy.Latest = rel.Latest.Name
		}
		if rel.EOLFrom != nil && *rel.EOLFrom != "" {
			t, err := time.Parse("2006-01-02", *rel.EOLFrom)
			if err != nil {
				return nil, fmt.Errorf("endoflife: GET %s: cycle %q: bad eolFrom %q: %w", u, rel.Name, *rel.EOLFrom, err)
			}
			cy.EOL = &t
		}
		cycles = append(cycles, cy)
	}
	return cycles, nil
}
