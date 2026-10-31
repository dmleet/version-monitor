package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfterHonoured(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient(10*time.Second, 2, 5*time.Second)
	start := time.Now()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || n.Load() != 2 {
		t.Errorf("status=%d calls=%d", resp.StatusCode, n.Load())
	}
	if el := time.Since(start); el < time.Second {
		t.Errorf("elapsed %v < 1s", el)
	}
}

func TestRetriesExhaustedReturnsResponse(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewClient(10*time.Second, 1, 10*time.Millisecond)
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || n.Load() != 2 {
		t.Errorf("status=%d calls=%d", resp.StatusCode, n.Load())
	}
}

func TestPostNotRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewClient(10*time.Second, 3, 10*time.Millisecond)
	resp, err := c.Post(srv.URL, "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n.Load() != 1 {
		t.Errorf("calls=%d, want 1", n.Load())
	}
}

func TestCancelledContextStopsRetry(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewClient(time.Minute, 5, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	start := time.Now()
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected error")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %v, want prompt stop", el)
	}
	if n.Load() != 1 {
		t.Errorf("calls=%d, want 1", n.Load())
	}
}
