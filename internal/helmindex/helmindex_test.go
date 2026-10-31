package helmindex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const fixture = `apiVersion: v1
generated: "2026-10-01T00:00:00Z"
entries:
  cert-manager:
    - name: cert-manager
      version: v1.21.3
      appVersion: v1.21.3
      urls: [https://x/a.tgz]
      maintainers:
        - name: someone
    - name: cert-manager
      version: v1.20.5
      appVersion: v1.20.5
  longhorn:
    - version: 1.11.0-rc.1
      appVersion: v1.11.0-rc.1
      keywords: [a, b]
    - version: 1.10.2
      appVersion: v1.10.2
    - version: 1.10.1
      appVersion: v1.10.1
`

func server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repo/index.yaml" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("User-Agent") != "version-monitor" || r.Header.Get("Accept") == "" {
			http.Error(w, "bad headers", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(fixture))
	}))
}

func TestIndexEntries(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), UserAgent: "version-monitor"}
	for _, u := range []string{srv.URL + "/repo", srv.URL + "/repo/"} {
		idx, err := c.Index(context.Background(), u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		es, ok := idx.Entries("longhorn")
		if !ok {
			t.Fatal("longhorn missing")
		}
		want := []Entry{{"1.11.0-rc.1", "v1.11.0-rc.1"}, {"1.10.2", "v1.10.2"}, {"1.10.1", "v1.10.1"}}
		if !reflect.DeepEqual(es, want) {
			t.Errorf("got %v want %v", es, want)
		}
		cm, _ := idx.Entries("cert-manager")
		if len(cm) != 2 || cm[0].Version != "v1.21.3" {
			t.Errorf("cert-manager: %v", cm)
		}
		if got := idx.Charts(); !reflect.DeepEqual(got, []string{"cert-manager", "longhorn"}) {
			t.Errorf("charts: %v", got)
		}
	}
}

func TestMissingChart(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	idx, err := c.Index(context.Background(), srv.URL+"/repo")
	if err != nil {
		t.Fatal(err)
	}
	if es, ok := idx.Entries("nope"); ok || es != nil {
		t.Errorf("expected missing, got %v %v", es, ok)
	}
}

func TestNotFound(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	_, err := c.Index(context.Background(), srv.URL+"/other")
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), srv.URL+"/other/index.yaml") {
		t.Fatalf("err = %v", err)
	}
}

func TestContextCancelled(t *testing.T) {
	srv := server(t)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Client{HTTP: srv.Client()}).Index(ctx, srv.URL+"/repo"); err == nil {
		t.Fatal("expected error")
	}
}
