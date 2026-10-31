// Package helmindex fetches a Helm repository's index.yaml and answers
// "which versions of chart X exist".
package helmindex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Client fetches Helm repository indexes.
type Client struct {
	HTTP      *http.Client
	UserAgent string
}

// Entry is one published chart version.
type Entry struct {
	Version    string // as published, e.g. "1.10.2" or "v1.21.3"
	AppVersion string
}

// Index is a decoded repository index (only chart versions are kept).
type Index struct {
	entries map[string][]Entry
}

type rawEntry struct {
	Version    string `yaml:"version"`
	AppVersion string `yaml:"appVersion"`
}

type rawIndex struct {
	Entries map[string][]rawEntry `yaml:"entries"`
}

// Index fetches <repoURL>/index.yaml (repoURL with or without a trailing
// slash). The caller fetches each repo once per run and reuses the Index.
// maxBody bounds an index.yaml read; the largest public index we use is
// ~7 MB.
const maxBody = 64 << 20

func (c *Client) Index(ctx context.Context, repoURL string) (*Index, error) {
	url := strings.TrimRight(repoURL, "/") + "/index.yaml"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("helmindex: build request for %s: %w", url, err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "version-monitor"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/x-yaml, application/yaml, text/yaml, text/plain, */*")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("helmindex: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("helmindex: GET %s: unexpected HTTP status %d", url, resp.StatusCode)
	}
	var raw rawIndex
	if err := yaml.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("helmindex: decode %s (HTTP %d): %w", url, resp.StatusCode, err)
	}
	idx := &Index{entries: make(map[string][]Entry, len(raw.Entries))}
	for name, list := range raw.Entries {
		es := make([]Entry, 0, len(list))
		for _, r := range list {
			es = append(es, Entry(r))
		}
		idx.entries[name] = es
	}
	return idx, nil
}

// Entries returns all published entries for a chart, in index order.
// ok is false when the chart is not in the index.
func (i *Index) Entries(chart string) (entries []Entry, ok bool) {
	es, ok := i.entries[chart]
	if !ok {
		return nil, false
	}
	return append([]Entry(nil), es...), true
}

// Charts returns the chart names in the index (sorted), for diagnostics.
func (i *Index) Charts() []string {
	names := make([]string, 0, len(i.entries))
	for n := range i.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
