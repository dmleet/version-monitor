package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const fixture = `[
 {"tag_name":"v1.15.0-alpha.2","draft":false,"prerelease":true,"id":1},
 {"tag_name":"v1.14.5","draft":false,"prerelease":false,"id":2},
 {"tag_name":"v1.14.6","draft":true,"prerelease":false,"id":3},
 {"tag_name":"v1.13.12","draft":false,"prerelease":false,"id":4}]`

func TestReleases(t *testing.T) {
	var gotAuth, gotAccept, gotUA, gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccept, gotUA, gotURI = r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("User-Agent"), r.URL.RequestURI()
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, UserAgent: "version-monitor", Token: "tok"}
	tags, err := c.Releases(context.Background(), "siderolabs/talos")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"v1.14.5", "v1.13.12"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("got %v want %v", tags, want)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotAccept != "application/vnd.github+json" || gotUA != "version-monitor" {
		t.Errorf("headers accept=%q ua=%q", gotAccept, gotUA)
	}
	if gotURI != "/repos/siderolabs/talos/releases?per_page=100" {
		t.Errorf("uri = %q", gotURI)
	}
}

func TestNoTokenNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			http.Error(w, "unexpected auth", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	if _, err := c.Releases(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimit(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1793000000")
			w.WriteHeader(code)
		}))
		c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
		_, err := c.Releases(context.Background(), "a/b")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "1793000000") {
			t.Errorf("code %d: err = %v", code, err)
		}
	}
}

func TestOtherErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	_, err := c.Releases(context.Background(), "a/b")
	if err == nil || !strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("err = %v", err)
	}
}
