// Package github lists release tags from the GitHub REST API.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DefaultBaseURL is the GitHub REST API root.
const DefaultBaseURL = "https://api.github.com"

// Client queries the GitHub releases API.
type Client struct {
	HTTP      *http.Client
	BaseURL   string // default DefaultBaseURL
	UserAgent string
	Token     string // optional; sent as a Bearer token when non-empty
}

type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Releases returns tag names of releases that are neither draft nor
// prerelease, in API order (newest first). One request, no pagination.
// maxBody bounds a releases response: 100 releases with full notes and
// asset lists can exceed 10 MB (siderolabs/talos does).
const maxBody = 64 << 20

func (c *Client) Releases(ctx context.Context, repo string) ([]string, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u := strings.TrimRight(base, "/") + "/repos/" + repo + "/releases?per_page=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("github: build request for %s: %w", u, err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "version-monitor"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
			resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return nil, fmt.Errorf("github: GET %s: rate limit exceeded (HTTP status %d, X-RateLimit-Reset=%s)",
				u, resp.StatusCode, resp.Header.Get("X-RateLimit-Reset"))
		}
		return nil, fmt.Errorf("github: GET %s: unexpected HTTP status %d", u, resp.StatusCode)
	}
	var rels []release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&rels); err != nil {
		return nil, fmt.Errorf("github: decode %s (HTTP %d): %w", u, resp.StatusCode, err)
	}
	tags := make([]string, 0, len(rels))
	for _, r := range rels {
		if r.Draft || r.Prerelease || r.TagName == "" {
			continue
		}
		tags = append(tags, r.TagName)
	}
	return tags, nil
}
