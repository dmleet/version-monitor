package endoflife

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fixture = `{"schema_version":"1.2.0","result":{"name":"kubernetes","releases":[
 {"name":"1.37","releaseDate":"2026-08-26","isEol":false,"eolFrom":"2027-10-28","latest":{"name":"1.37.2"}},
 {"name":"3.7","isEol":false,"eolFrom":null,"latest":{"name":"3.7.17"}},
 {"name":"17","isEol":true,"eolFrom":"2029-11-08","latest":{"name":"17.11"}},
 {"name":"0.1","isEol":false,"eolFrom":null,"latest":null}]}}`

func newServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/products/kubernetes" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("User-Agent") != "version-monitor" || r.Header.Get("Accept") == "" {
			http.Error(w, "bad headers", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestProduct(t *testing.T) {
	srv := newServer(fixture)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, UserAgent: "version-monitor"}
	cs, err := c.Product(context.Background(), "kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 4 {
		t.Fatalf("len = %d", len(cs))
	}
	want := time.Date(2027, 10, 28, 0, 0, 0, 0, time.UTC)
	if cs[0].Name != "1.37" || cs[0].Latest != "1.37.2" || cs[0].EOL == nil || !cs[0].EOL.Equal(want) || cs[0].IsEOL {
		t.Errorf("cs[0] = %+v", cs[0])
	}
	if cs[1].EOL != nil || cs[1].Latest != "3.7.17" {
		t.Errorf("cs[1] = %+v", cs[1])
	}
	if cs[2].EOL == nil || cs[2].EOL.Year() != 2029 || !cs[2].IsEOL {
		t.Errorf("cs[2] = %+v", cs[2])
	}
	if cs[3].Latest != "" || cs[3].EOL != nil {
		t.Errorf("cs[3] = %+v", cs[3])
	}
}

func TestProductNotFound(t *testing.T) {
	srv := newServer(fixture)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL + "/"}
	_, err := c.Product(context.Background(), "nosuch")
	if err == nil || !strings.Contains(err.Error(), "nosuch") || !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
}

func TestProductBadDate(t *testing.T) {
	srv := newServer(`{"result":{"releases":[{"name":"1","isEol":false,"eolFrom":"soon","latest":null}]}}`)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	if _, err := c.Product(context.Background(), "kubernetes"); err == nil || !strings.Contains(err.Error(), "soon") {
		t.Fatalf("err = %v", err)
	}
}

func TestProductMalformedJSON(t *testing.T) {
	srv := newServer(`{`)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	if _, err := c.Product(context.Background(), "kubernetes"); err == nil {
		t.Fatal("expected error")
	}
}
