// Package registry lists the tags of container image repositories
// anonymously. It never pulls images.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dmleet/version-monitor/internal/model"
)

// Client lists image tags from Docker Hub, quay.io and OCI registries.
type Client struct {
	HTTP           *http.Client // required; caller sets timeouts
	UserAgent      string       // default "version-monitor"
	MaxPages       int          // OCI/quay page cap, default 30
	DockerHubPages int          // default 5
	HubBase        string       // default "https://hub.docker.com"
	QuayBase       string       // default "https://quay.io"
	// HostOverride maps a registry host (e.g. "ghcr.io") to a base URL
	// including scheme, so tests can stand in for a real registry. Also
	// applies to the token realm host.
	HostOverride map[string]string
}

// New returns a Client with defaults.
func New(httpClient *http.Client) *Client {
	return &Client{
		HTTP:           httpClient,
		UserAgent:      "version-monitor",
		MaxPages:       30,
		DockerHubPages: 5,
		HubBase:        "https://hub.docker.com",
		QuayBase:       "https://quay.io",
	}
}

func (c *Client) maxPages() int {
	if c.MaxPages > 0 {
		return c.MaxPages
	}
	return 30
}

func (c *Client) hubPages() int {
	if c.DockerHubPages > 0 {
		return c.DockerHubPages
	}
	return 5
}

func orDefault(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

// Normalise splits an image reference into repository, tag and digest.
func Normalise(image string) (repo, tag, digest string) {
	if i := strings.Index(image, "@"); i >= 0 {
		digest = image[i+1:]
		image = image[:i]
	}
	// A colon after the last slash is a tag separator; before it, a port.
	if i := strings.LastIndex(image, ":"); i >= 0 && i > strings.LastIndex(image, "/") {
		tag = image[i+1:]
		image = image[:i]
	}
	image = strings.TrimPrefix(image, "docker.io/")
	if strings.HasPrefix(image, "library/") && strings.Count(image, "/") == 1 {
		image = strings.TrimPrefix(image, "library/")
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return image, tag, digest
}

// Tags returns all tags of a normalised repository, in no guaranteed order.
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error) {
	orig := repo
	if strings.HasPrefix(repo, "lscr.io/") {
		repo = "ghcr.io/" + strings.TrimPrefix(repo, "lscr.io/")
	}
	host := model.RegistryHost(repo)
	var tags []string
	var err error
	switch host {
	case "docker.io":
		tags, err = c.hubTags(ctx, repo)
	case "quay.io":
		tags, err = c.quayTags(ctx, strings.TrimPrefix(repo, "quay.io/"))
	default:
		tags, err = c.ociTags(ctx, host, strings.TrimPrefix(repo, host+"/"))
	}
	if err != nil {
		return nil, fmt.Errorf("listing tags of %s: %w", orig, err)
	}
	return tags, nil
}

type statusError struct {
	URL    string
	Status int
	Header http.Header
	Body   string
}

func (e *statusError) Error() string {
	if e.Status == http.StatusTooManyRequests {
		return fmt.Sprintf("rate limited (HTTP 429) by %s", e.URL)
	}
	return fmt.Sprintf("HTTP %d from %s: %s", e.Status, e.URL, e.Body)
}

// maxBody bounds every response body read.
const maxBody = 32 << 20

func (c *Client) get(ctx context.Context, u string, hdr map[string]string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("building request for %s: %w", u, err)
	}
	req.Header.Set("User-Agent", orDefault(c.UserAgent, "version-monitor"))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", u, err)
	}
	if len(body) > maxBody {
		return nil, nil, fmt.Errorf("response from %s exceeds %d bytes", u, maxBody)
	}
	if resp.StatusCode != http.StatusOK {
		s := string(body)
		if len(s) > 200 {
			s = s[:200]
		}
		return nil, resp.Header, &statusError{URL: u, Status: resp.StatusCode, Header: resp.Header, Body: s}
	}
	return body, resp.Header, nil
}

var jsonHdr = map[string]string{"Accept": "application/json"}

func (c *Client) hubTags(ctx context.Context, repo string) ([]string, error) {
	ns, name := "library", repo
	if i := strings.Index(repo, "/"); i >= 0 {
		ns, name = repo[:i], repo[i+1:]
	}
	next := fmt.Sprintf("%s/v2/repositories/%s/%s/tags?page_size=100&ordering=last_updated",
		strings.TrimRight(orDefault(c.HubBase, "https://hub.docker.com"), "/"), ns, name)
	var tags []string
	for page := 0; page < c.hubPages() && next != ""; page++ {
		body, _, err := c.get(ctx, next, jsonHdr)
		if err != nil {
			return nil, err
		}
		var r struct {
			Next    string `json:"next"`
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", next, err)
		}
		for _, t := range r.Results {
			tags = append(tags, t.Name)
		}
		next = r.Next
	}
	return tags, nil
}

func (c *Client) quayTags(ctx context.Context, path string) ([]string, error) {
	base := strings.TrimRight(orDefault(c.HostOverride["quay.io"], orDefault(c.QuayBase, "https://quay.io")), "/")
	var tags []string
	for page := 1; page <= c.maxPages(); page++ {
		u := fmt.Sprintf("%s/api/v1/repository/%s/tag/?limit=100&page=%d&onlyActiveTags=true", base, path, page)
		body, _, err := c.get(ctx, u, jsonHdr)
		if err != nil {
			return nil, err
		}
		var r struct {
			Tags []struct {
				Name string `json:"name"`
			} `json:"tags"`
			HasAdditional bool `json:"has_additional"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", u, err)
		}
		for _, t := range r.Tags {
			tags = append(tags, t.Name)
		}
		if !r.HasAdditional {
			return tags, nil
		}
	}
	return nil, fmt.Errorf("more than %d pages; raise MaxPages or use a github_releases override", c.maxPages())
}

func (c *Client) baseFor(host string) string {
	if b, ok := c.HostOverride[host]; ok {
		return strings.TrimRight(b, "/")
	}
	return "https://" + host
}

func (c *Client) ociTags(ctx context.Context, host, path string) ([]string, error) {
	next := fmt.Sprintf("%s/v2/%s/tags/list?n=1000", c.baseFor(host), path)
	first, err := url.Parse(next)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", next, err)
	}
	var tags []string
	var token string
	for page := 0; page < c.maxPages(); page++ {
		hdr := map[string]string{"Accept": "application/json"}
		if token != "" {
			hdr["Authorization"] = "Bearer " + token
		}
		body, h, err := c.get(ctx, next, hdr)
		var se *statusError
		if errors.As(err, &se) && se.Status == http.StatusUnauthorized && token == "" {
			token, err = c.fetchToken(ctx, se.Header.Get("Www-Authenticate"), path)
			if err != nil {
				return nil, fmt.Errorf("authenticating for %s: %w", next, err)
			}
			hdr["Authorization"] = "Bearer " + token
			body, h, err = c.get(ctx, next, hdr)
		}
		if err != nil {
			return nil, err
		}
		var r struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", next, err)
		}
		tags = append(tags, r.Tags...)
		link := nextLink(h.Get("Link"))
		if link == "" {
			return tags, nil
		}
		base, err := url.Parse(next)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", next, err)
		}
		ref, err := url.Parse(link)
		if err != nil {
			return nil, fmt.Errorf("parsing Link %q from %s: %w", link, next, err)
		}
		resolved := base.ResolveReference(ref)
		if resolved.Scheme != first.Scheme || resolved.Host != first.Host {
			return nil, fmt.Errorf("next Link %q points at unexpected host %s://%s (registry is %s://%s)",
				link, resolved.Scheme, resolved.Host, first.Scheme, first.Host)
		}
		next = resolved.String()
	}
	return nil, fmt.Errorf("more than %d pages; raise MaxPages or use a github_releases override", c.maxPages())
}

// nextLink extracts the target of a rel="next" Link header value.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "<") {
			continue
		}
		end := strings.Index(part, ">")
		if end < 0 {
			continue
		}
		params := strings.ToLower(part[end+1:])
		if strings.Contains(params, `rel="next"`) || strings.Contains(params, "rel=next") {
			return part[1:end]
		}
	}
	return ""
}

func (c *Client) fetchToken(ctx context.Context, challenge, path string) (string, error) {
	ch := strings.TrimSpace(challenge)
	if len(ch) < 7 || !strings.EqualFold(ch[:7], "bearer ") {
		return "", fmt.Errorf("unsupported WWW-Authenticate %q", challenge)
	}
	params := parseParams(ch[7:])
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("no realm in WWW-Authenticate %q", challenge)
	}
	scope := orDefault(params["scope"], "repository:"+path+":pull")
	ru, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("bad realm %q: %w", realm, err)
	}
	ov, overridden := c.HostOverride[ru.Host]
	if ru.Scheme != "https" && !overridden {
		return "", fmt.Errorf("refusing token realm %q: scheme must be https (host %s is not overridden)", realm, ru.Host)
	}
	if overridden {
		ou, err := url.Parse(ov)
		if err != nil {
			return "", fmt.Errorf("bad host override %q: %w", ov, err)
		}
		ru.Scheme, ru.Host = ou.Scheme, ou.Host
	}
	q := ru.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", scope)
	ru.RawQuery = q.Encode()
	body, _, err := c.get(ctx, ru.String(), jsonHdr)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return "", fmt.Errorf("decoding token from %s: %w", ru.String(), err)
	}
	tok := orDefault(t.Token, t.AccessToken)
	if tok == "" {
		return "", fmt.Errorf("no token in response from %s", ru.String())
	}
	return tok, nil
}

// parseParams parses key="value",key2="value2" lists.
func parseParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,")
		eq := strings.Index(s, "=")
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := strings.Index(s[1:], `"`)
			if end < 0 {
				val, s = s[1:], ""
			} else {
				val, s = s[1:1+end], s[end+2:]
			}
		} else {
			end := strings.Index(s, ",")
			if end < 0 {
				val, s = s, ""
			} else {
				val, s = s[:end], s[end+1:]
			}
		}
		out[key] = val
	}
	return out
}
