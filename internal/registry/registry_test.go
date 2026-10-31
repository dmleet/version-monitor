package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestNormalise(t *testing.T) {
	tests := []struct{ in, repo, tag, digest string }{
		{"docker.io/library/redis:7.4.2-alpine3.21", "redis", "7.4.2-alpine3.21", ""},
		{"docker.io/grafana/grafana:13.2.5-distroless", "grafana/grafana", "13.2.5-distroless", ""},
		{"ghcr.io/cloudnative-pg/postgresql:17.11-202610190957-system-bookworm@sha256:abc", "ghcr.io/cloudnative-pg/postgresql", "17.11-202610190957-system-bookworm", "sha256:abc"},
		{"alpine@sha256:abc", "alpine", "", "sha256:abc"},
		{"redis", "redis", "latest", ""},
		{"grafana/grafana", "grafana/grafana", "latest", ""},
		{"lscr.io/linuxserver/radarr:6.1.1.10360-ls303", "lscr.io/linuxserver/radarr", "6.1.1.10360-ls303", ""},
		{"registry.k8s.io/kube-apiserver:v1.35.10", "registry.k8s.io/kube-apiserver", "v1.35.10", ""},
		{"myreg.home.arpa:5000/foo:1.2", "myreg.home.arpa:5000/foo", "1.2", ""},
		{"myreg.home.arpa:5000/foo", "myreg.home.arpa:5000/foo", "latest", ""},
	}
	for _, tc := range tests {
		r, tg, d := Normalise(tc.in)
		if r != tc.repo || tg != tc.tag || d != tc.digest {
			t.Errorf("Normalise(%q) = (%q,%q,%q), want (%q,%q,%q)", tc.in, r, tg, d, tc.repo, tc.tag, tc.digest)
		}
	}
}

func sorted(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }

func TestDockerHubPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "version-monitor" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		if r.URL.Path != "/v2/repositories/library/redis/tags" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"count":3,"next":null,"results":[{"name":"c"}]}`)
			return
		}
		fmt.Fprintf(w, `{"count":3,"next":"%s/v2/repositories/library/redis/tags?page=2","results":[{"name":"a"},{"name":"b"}]}`, srv.URL)
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.HubBase = srv.URL
	got, err := c.Tags(context.Background(), "redis")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sorted(got), []string{"a", "b", "c"}) {
		t.Errorf("got %v", got)
	}
}

func TestDockerHubRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.HubBase = srv.URL
	_, err := c.Tags(context.Background(), "grafana/grafana")
	if err == nil || !strings.Contains(err.Error(), "rate") {
		t.Errorf("err = %v, want rate limit error", err)
	}
}

func TestQuayPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repository/prometheus/prometheus/tag/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			fmt.Fprint(w, `{"tags":[{"name":"v3.15.1"}],"has_additional":true}`)
		case "2":
			fmt.Fprint(w, `{"tags":[{"name":"v3.15.0"}],"has_additional":false}`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.QuayBase = srv.URL
	got, err := c.Tags(context.Background(), "quay.io/prometheus/prometheus")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sorted(got), []string{"v3.15.0", "v3.15.1"}) {
		t.Errorf("got %v", got)
	}
}

func TestOCIAuthAndPaginationWithLSCRAlias(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.URL.Query().Get("service") != "ghcr.io" || r.URL.Query().Get("scope") != "repository:linuxserver/radarr:pull" {
				t.Errorf("token query = %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"token":"sekret"}`)
		case "/v2/linuxserver/radarr/tags/list":
			if r.Header.Get("Authorization") != "Bearer sekret" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:linuxserver/radarr:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("last") == "xyz" {
				fmt.Fprint(w, `{"name":"linuxserver/radarr","tags":["3","4"]}`)
				return
			}
			w.Header().Set("Link", `</v2/linuxserver/radarr/tags/list?n=1000&last=xyz>; rel="next"`)
			fmt.Fprint(w, `{"name":"linuxserver/radarr","tags":["1","2"]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.HostOverride = map[string]string{"ghcr.io": srv.URL}
	got, err := c.Tags(context.Background(), "lscr.io/linuxserver/radarr")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sorted(got), []string{"1", "2", "3", "4"}) {
		t.Errorf("got %v", got)
	}
}

func TestOCIAnonymous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/kube-apiserver/tags/list" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"name":"kube-apiserver","tags":["v1.35.10"]}`)
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.HostOverride = map[string]string{"registry.k8s.io": srv.URL}
	got, err := c.Tags(context.Background(), "registry.k8s.io/kube-apiserver")
	if err != nil || !slices.Equal(got, []string{"v1.35.10"}) {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestOCIPageCapErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `</v2/foo/tags/list?n=1000&last=x>; rel="next"`)
		fmt.Fprint(w, `{"tags":["1"]}`)
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.MaxPages = 2
	c.HostOverride = map[string]string{"reg.example": srv.URL}
	_, err := c.Tags(context.Background(), "reg.example/foo")
	if err == nil || !strings.Contains(err.Error(), "more than 2 pages") {
		t.Errorf("err = %v, want page cap error", err)
	}
}

func TestQuayPageCapErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tags":[{"name":"v1"}],"has_additional":true}`)
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.MaxPages = 2
	c.QuayBase = srv.URL
	_, err := c.Tags(context.Background(), "quay.io/a/b")
	if err == nil || !strings.Contains(err.Error(), "more than 2 pages") {
		t.Errorf("err = %v, want page cap error", err)
	}
}

func TestOCIRejectsOffHostNextLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<http://evil.example/v2/foo/tags/list?last=x>; rel="next"`)
		fmt.Fprint(w, `{"tags":["1"]}`)
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.HostOverride = map[string]string{"reg.example": srv.URL}
	_, err := c.Tags(context.Background(), "reg.example/foo")
	if err == nil || !strings.Contains(err.Error(), "evil.example") {
		t.Errorf("err = %v, want unexpected host error", err)
	}
}

func TestOCIRejectsInsecureRealm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://auth.example/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.HostOverride = map[string]string{"reg.example": srv.URL}
	_, err := c.Tags(context.Background(), "reg.example/foo")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v, want https realm error", err)
	}
}

func TestOversizedBodyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tags":["`))
		buf := make([]byte, 1<<20)
		for i := range buf {
			buf[i] = 'a'
		}
		for range 33 {
			_, _ = w.Write(buf)
		}
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.HostOverride = map[string]string{"reg.example": srv.URL}
	_, err := c.Tags(context.Background(), "reg.example/foo")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want size error", err)
	}
}
