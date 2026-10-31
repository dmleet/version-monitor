// Package httpx provides an HTTP client that retries idempotent requests on
// transient failures.
package httpx

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// NewClient returns an *http.Client with the given timeout whose transport
// retries idempotent requests (GET/HEAD only) on 429, 502, 503, 504 and on
// transient network errors, at most retries times. It honours a Retry-After
// header (seconds or HTTP-date) capped at maxWait, otherwise backs off 1s,
// 2s, 4s... capped at maxWait. It never retries when the request context is
// done. Responses that are not retried are returned as-is (the caller still
// sees the 429/5xx on final failure).
func NewClient(timeout time.Duration, retries int, maxWait time.Duration) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Timeout:   timeout,
		Transport: &retryTransport{base: base, retries: retries, maxWait: maxWait},
	}
}

type retryTransport struct {
	base    http.RoundTripper
	retries int
	maxWait time.Duration
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return t.base.RoundTrip(req)
	}
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if ctx.Err() != nil {
			if err == nil {
				resp.Body.Close()
			}
			return nil, ctx.Err()
		}
		if attempt >= t.retries || (err == nil && !retryableStatus(resp.StatusCode)) {
			return resp, err
		}
		wait := t.backoff(attempt)
		if err == nil {
			if ra, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
				wait = min(ra, t.maxWait)
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (t *retryTransport) backoff(attempt int) time.Duration {
	d := time.Second
	for range attempt {
		d *= 2
		if d >= t.maxWait {
			break
		}
	}
	return min(d, t.maxWait)
}

func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}
