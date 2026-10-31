package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmleet/version-monitor/internal/cluster"
	"github.com/dmleet/version-monitor/internal/config"
	"github.com/dmleet/version-monitor/internal/helmindex"
	"github.com/dmleet/version-monitor/internal/model"
)

const testConfig = `
defaults: {unit: minor, max_lines_behind: 1}
exclude_namespaces: [kube-system, flux-system, longhorn-system]
eol:
  - {product: kubernetes, source: kubernetes}
  - {product: flux, source: flux}
  - {product: longhorn, helmrelease: longhorn-system/longhorn}
  - {product: grafana, image: grafana/grafana}
  - {product: postgresql, image: [postgres]}
  - {product: nothing-runs-this, image: [ghost/ghost]}
overrides:
  traefik: {unit: major}
  tandoor-recipes/postgres: {unit: major, pin: major}
  "lscr.io/linuxserver/*": {tag_regex: '^(?P<v>\d+\.\d+\.\d+\.\d+)-ls\d+$', github_releases: "linuxserver/docker-{name}"}
  "dmleet/*": {ignore: true}
`

type fakeCluster struct {
	inv *cluster.Inventory
	err error
}

func (f fakeCluster) Inventory(context.Context) (*cluster.Inventory, error) { return f.inv, f.err }

type fakeRegistry struct {
	mu    sync.Mutex
	tags  map[string][]string
	calls map[string]int
}

func (f *fakeRegistry) Tags(_ context.Context, repo string) ([]string, error) {
	f.mu.Lock()
	f.calls[repo]++
	f.mu.Unlock()
	t, ok := f.tags[repo]
	if !ok {
		return nil, errors.New("registry down for " + repo)
	}
	return t, nil
}

type fakeHelm struct {
	mu    sync.Mutex
	srv   *httptest.Server
	calls int
}

func (f *fakeHelm) Index(ctx context.Context, url string) (*helmindex.Index, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return (&helmindex.Client{HTTP: f.srv.Client()}).Index(ctx, f.srv.URL+"/"+strings.TrimPrefix(url, "https://"))
}

type fakeEOL struct{ products map[string][]model.Cycle }

func (f fakeEOL) Product(_ context.Context, name string) ([]model.Cycle, error) {
	c, ok := f.products[name]
	if !ok {
		return nil, errors.New("unknown product " + name)
	}
	return c, nil
}

type fakeGitHub struct{ releases map[string][]string }

func (f fakeGitHub) Releases(_ context.Context, repo string) ([]string, error) {
	return f.releases[repo], nil
}

func date(s string) *time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return &t
}

const longhornIndex = `apiVersion: v1
entries:
  longhorn:
    - {version: 1.13.0, appVersion: v1.13.0}
    - {version: 1.12.1, appVersion: v1.12.1}
    - {version: 1.11.3, appVersion: v1.11.3}
    - {version: 1.10.2, appVersion: v1.10.2}
    - {version: 1.14.0-rc.1, appVersion: v1.14.0-rc.1}
`
const traefikIndex = `apiVersion: v1
entries:
  traefik:
    - {version: 42.2.0, appVersion: v3.7.16}
    - {version: 41.0.0, appVersion: v3.6.0}
    - {version: 40.3.0, appVersion: v3.7.4}
`

func newMonitor(t *testing.T, inv *cluster.Inventory) (*Monitor, *fakeRegistry, *fakeHelm) {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/charts.longhorn.io/index.yaml", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, longhornIndex) })
	mux.HandleFunc("/traefik.github.io/charts/index.yaml", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, traefikIndex) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	reg := &fakeRegistry{calls: map[string]int{}, tags: map[string][]string{
		"lscr.io/linuxserver/radarr": {"6.4.4.10685-ls324", "6.3.0.10600-ls320", "6.2.0.10500-ls310", "6.1.1.10360-ls303", "nightly-6.5.0.1-ls1", "amd64-6.4.4.10685-ls324"},
		"postgres":                   {"18.1-alpine3.23", "17.11-alpine3.23", "16.15-alpine3.24", "16.11-alpine3.23"},
		"grafana/grafana":            {"13.3.0-distroless", "13.2.5-distroless"},
	}}
	helm := &fakeHelm{srv: srv}
	m := &Monitor{
		Config:   cfg,
		Cluster:  fakeCluster{inv: inv},
		Registry: reg,
		Helm:     helm,
		EndOfLife: fakeEOL{products: map[string][]model.Cycle{
			"kubernetes": {{Name: "1.37", Latest: "1.37.2"}, {Name: "1.36", Latest: "1.36.6"}, {Name: "1.35", Latest: "1.35.10", EOL: date("2027-02-28")}},
			"flux":       {{Name: "2.10", Latest: "2.10.0"}, {Name: "2.8", Latest: "2.8.8"}},
			"longhorn":   {{Name: "1.13"}, {Name: "1.10", Latest: "1.10.2", EOL: date("2027-03-25")}},
			"grafana":    {{Name: "13.2", EOL: date("2027-05-18")}},
			"postgresql": {{Name: "17", EOL: date("2029-11-08")}, {Name: "16", EOL: date("2028-11-09")}},
		}},
		GitHub: fakeGitHub{releases: map[string][]string{
			"siderolabs/talos":          {"v1.14.5", "v1.13.12"},
			"linuxserver/docker-radarr": {"6.4.4.10685-ls324", "6.3.0.10600-ls320", "6.2.0.10500-ls310", "6.1.1.10360-ls303"},
			"fluxcd/flux2":              {"v2.10.0", "v2.9.6", "v2.8.8"},
		}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) },
	}
	return m, reg, helm
}

func inventory() *cluster.Inventory {
	return &cluster.Inventory{
		Nodes: []cluster.Node{
			{Name: "a", KubeletVersion: "v1.35.10", OSImage: "Talos (v1.13.12)"},
			{Name: "b", KubeletVersion: "v1.35.9", OSImage: "Talos (v1.13.12)"}, // mid-upgrade straggler: lowest wins
		},
		FluxVersion: "v2.8.8",
		HelmRepositories: []cluster.HelmRepository{
			{Namespace: "flux-system", Name: "longhorn", URL: "https://charts.longhorn.io"},
			{Namespace: "flux-system", Name: "traefik", URL: "https://traefik.github.io/charts"},
			{Namespace: "flux-system", Name: "oci-repo", URL: "oci://ghcr.io/x", Type: "oci"},
		},
		HelmReleases: []cluster.HelmRelease{
			{Namespace: "longhorn-system", Name: "longhorn", Chart: "longhorn", SpecVersion: "1.10.2", ChartVersion: "1.10.2", AppVersion: "v1.10.2",
				SourceKind: "HelmRepository", SourceName: "longhorn", SourceNamespace: "flux-system"},
			{Namespace: "traefik", Name: "traefik", Chart: "traefik", SpecVersion: "40.3.0", ChartVersion: "40.3.0", AppVersion: "v3.7.4",
				SourceKind: "HelmRepository", SourceName: "traefik", SourceNamespace: "flux-system"},
			{Namespace: "x", Name: "from-oci", Chart: "c", SpecVersion: "1.0.0", ChartVersion: "1.0.0",
				SourceKind: "HelmRepository", SourceName: "oci-repo", SourceNamespace: "flux-system"},
		},
		Workloads: []cluster.Workload{
			{Kind: "Deployment", Namespace: "radarr", Name: "radarr", Images: []string{"lscr.io/linuxserver/radarr:6.1.1.10360-ls303"}},
			{Kind: "StatefulSet", Namespace: "tandoor-recipes", Name: "postgres", Images: []string{"postgres:16.11-alpine3.23"}},
			// Same repo, different tag, same EOL cycle: must not duplicate the eol series.
			{Kind: "CronJob", Namespace: "tandoor-recipes", Name: "pg-backup", Images: []string{"postgres:16-alpine"}},
			{Kind: "Deployment", Namespace: "monitoring", Name: "kube-prometheus-stack-grafana", HelmRelease: "kube-prometheus-stack",
				Images: []string{"docker.io/grafana/grafana:13.2.5-distroless", "quay.io/kiwigrid/k8s-sidecar:2.14.1"}},
			{Kind: "Deployment", Namespace: "beets", Name: "beets", Images: []string{"dmleet/beets-hermes:2.14.0-8fad9d3"}},
			{Kind: "StatefulSet", Namespace: "valheim", Name: "valheim", Images: []string{"ghcr.io/community-valheim-tools/valheim-server:sha-e36cbfb", "alpine@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"}},
			{Kind: "Deployment", Namespace: "longhorn-system", Name: "csi-attacher", Images: []string{"longhornio/csi-attacher:v4.10.0-20251226"}},
			{Kind: "Deployment", Namespace: "kube-system", Name: "coredns", Images: []string{"registry.k8s.io/coredns/coredns:v1.14.7"}},
			{Kind: "CronJob", Namespace: "radarr", Name: "dup", Images: []string{"lscr.io/linuxserver/radarr:6.1.1.10360-ls303"}},
			{Kind: "Deployment", Namespace: "down", Name: "down", Images: []string{"example/unreachable:1.0.0"}},
		},
	}
}

// byID indexes results by component ID (first occurrence wins — the same
// repo at a second tag is a second result) and by "ID@tag".
func byID(s *model.Snapshot) map[string]model.Result {
	m := map[string]model.Result{}
	for _, r := range s.Results {
		if _, dup := m[r.ID()]; !dup {
			m[r.ID()] = r
		}
		m[r.ID()+"@"+r.Current] = r
	}
	return m
}

func TestRun(t *testing.T) {
	m, reg, helm := newMonitor(t, inventory())
	snap, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := byID(snap)

	want := map[string]struct {
		status model.Status
		lines  int
		latest string
		behind bool
	}{
		"helmrelease:longhorn-system/longhorn":                         {model.StatusOK, 3, "1.13.0", true},
		"helmrelease:traefik/traefik":                                  {model.StatusOK, 2, "42.2.0", true}, // major unit via override
		"helmrelease:x/from-oci":                                       {model.StatusError, 0, "", false},
		"image:radarr/lscr.io/linuxserver/radarr":                      {model.StatusOK, 3, "6.4.4.10685-ls324", true},
		"image:tandoor-recipes/postgres":                               {model.StatusOK, 0, "16.15-alpine3.24", false},
		"image:beets/dmleet/beets-hermes":                              {model.StatusIgnored, 0, "", false},
		"image:valheim/ghcr.io/community-valheim-tools/valheim-server": {model.StatusSkipped, 0, "", false},
		"image:down/example/unreachable":                               {model.StatusError, 0, "", false},
		"kubernetes:kubernetes":                                        {model.StatusOK, 2, "1.37.2", true},
		"talos:talos":                                                  {model.StatusOK, 1, "v1.14.5", false},
		"flux:flux":                                                    {model.StatusOK, 2, "v2.10.0", true},
	}
	for id, w := range want {
		got, ok := r[id]
		if !ok {
			t.Errorf("%s: missing from results", id)
			continue
		}
		if got.Status != w.status || got.LinesBehind != w.lines || got.Latest != w.latest || got.BehindPolicy() != w.behind {
			t.Errorf("%s: got status=%s lines=%d latest=%q behind=%v err=%v; want %+v", id, got.Status, got.LinesBehind, got.Latest, got.BehindPolicy(), got.Err, w)
		}
	}
	for _, absent := range []string{
		"image:monitoring/grafana/grafana",              // helm-managed: EOL only
		"image:monitoring/quay.io/kiwigrid/k8s-sidecar", // helm-managed
		"image:longhorn-system/longhornio/csi-attacher", // excluded namespace
		"image:kube-system/registry.k8s.io/coredns/coredns",
		"image:valheim/alpine", // digest-only
	} {
		if _, ok := r[absent]; ok {
			t.Errorf("%s should not be in results", absent)
		}
	}
	if r["image:tandoor-recipes/postgres"].Pinned != true {
		t.Error("pinned postgres should be flagged Pinned")
	}
	if got := r["image:tandoor-recipes/postgres@16-alpine"]; got.Status != model.StatusNoMatch {
		t.Errorf("second postgres tag should be its own result (nomatch: no '#-alpine' shaped candidates), got %+v", got)
	}
	if got := r["kubernetes:kubernetes"].Current; got != "v1.35.9" {
		t.Errorf("kubernetes current should be the lowest node (v1.35.9), got %s", got)
	}

	// Each upstream fetched once despite duplicates.
	if reg.calls["lscr.io/linuxserver/radarr"] != 0 {
		t.Errorf("radarr must come from GitHub releases; registry called %d times", reg.calls["lscr.io/linuxserver/radarr"])
	}
	if reg.calls["grafana/grafana"] != 0 {
		t.Error("helm-managed grafana image should not have its tags fetched")
	}
	if helm.calls != 2 {
		t.Errorf("helm indexes fetched %d times, want 2", helm.calls)
	}

	// check_success: docker.io failed (unreachable repo), everything else ok.
	for src, ok := range map[string]bool{"kubernetes": true, "endoflife": true, "github": true, "helm:longhorn": true, "helm:traefik": true, "registry:docker.io": false} {
		if got, present := snap.CheckSuccess[src]; !present || got != ok {
			t.Errorf("CheckSuccess[%s] = %v (present %v), want %v", src, got, present, ok)
		}
	}

	// EOL
	eol := map[string]model.EOLResult{}
	for _, e := range snap.EOL {
		eol[e.Product+"|"+e.Namespace+"/"+e.Name] = e
	}
	if e := eol["kubernetes|/kubernetes"]; e.Cycle != "1.35" || !e.HasEOL || int(e.Days(snap.FinishedAt)) != 119 {
		t.Errorf("kubernetes eol: %+v days=%v", e, e.Days(snap.FinishedAt))
	}
	if e := eol["flux|/flux"]; e.Cycle != "2.8" || e.HasEOL {
		t.Errorf("flux eol should match 2.8 with no date: %+v", e)
	}
	if e := eol["longhorn|longhorn-system/longhorn"]; e.Cycle != "1.10" || e.Version != "1.10.2" {
		t.Errorf("longhorn eol via appVersion: %+v", e)
	}
	if e := eol["grafana|monitoring/grafana/grafana"]; e.Cycle != "13.2" || !e.HasEOL {
		t.Errorf("grafana eol must come from the helm-managed image: %+v", e)
	}
	if e := eol["postgresql|tandoor-recipes/postgres"]; e.Cycle != "16" {
		t.Errorf("postgres eol: %+v", e)
	}
	pgEOL := 0
	for _, e := range snap.EOL {
		if e.Product == "postgresql" && e.Namespace == "tandoor-recipes" {
			pgEOL++
		}
	}
	if pgEOL != 1 {
		t.Errorf("postgres at two tags in one cycle produced %d eol results, want 1 (duplicate label sets fail the scrape)", pgEOL)
	}
	if _, ok := eol["nothing-runs-this|/"]; ok {
		t.Error("a product nothing runs must produce no EOL result")
	}
	for _, e := range snap.EOL {
		if e.Err != nil {
			t.Errorf("unexpected EOL error: %+v", e)
		}
	}
}

func TestRunInventoryFailure(t *testing.T) {
	m, _, _ := newMonitor(t, nil)
	m.Cluster = fakeCluster{err: errors.New("apiserver 503")}
	snap, err := m.Run(context.Background())
	var inv *ErrInventory
	if !errors.As(err, &inv) {
		t.Fatalf("want *ErrInventory, got %v", err)
	}
	if snap == nil || snap.CheckSuccess["kubernetes"] != false || len(snap.Results) != 0 {
		t.Errorf("snapshot on inventory failure: %+v", snap)
	}
}

func TestRunNoFluxNoTalos(t *testing.T) {
	inv := &cluster.Inventory{Nodes: []cluster.Node{{Name: "n", KubeletVersion: "v1.35.10", OSImage: "Ubuntu 24.04"}}}
	m, _, _ := newMonitor(t, inv)
	snap, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := byID(snap)
	if _, ok := r["talos:talos"]; ok {
		t.Error("no talos component expected on non-Talos nodes")
	}
	if _, ok := r["flux:flux"]; ok {
		t.Error("no flux component expected without a flux version")
	}
	if _, ok := r["kubernetes:kubernetes"]; !ok {
		t.Error("kubernetes component expected")
	}
}
