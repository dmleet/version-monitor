package cluster

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const nodesPage1 = `{"metadata":{"continue":"abc"},"items":[
 {"metadata":{"name":"cp-1"},"status":{"nodeInfo":{"kubeletVersion":"v1.35.10","osImage":"Talos (v1.13.12)"}}}]}`
const nodesPage2 = `{"metadata":{},"items":[
 {"metadata":{"name":"w-1"},"status":{"nodeInfo":{"kubeletVersion":"v1.35.9","osImage":"Talos (v1.13.11)"}}}]}`

const deployments = `{"metadata":{},"items":[
 {"metadata":{"name":"grafana","namespace":"monitoring","labels":{"helm.toolkit.fluxcd.io/name":"kube-prometheus-stack"}},
  "spec":{"template":{"spec":{
    "initContainers":[{"image":"busybox:1.37"}],
    "containers":[{"image":"grafana/grafana:12.0.0"},{"image":"quay.io/kiwigrid/k8s-sidecar:1.30.0"},{"image":"busybox:1.37"}]}}}},
 {"metadata":{"name":"radarr","namespace":"radarr"},
  "spec":{"template":{"spec":{"containers":[{"image":"lscr.io/linuxserver/radarr:6.1.1.10360-ls303"}]}}}}]}`

const statefulsets = `{"metadata":{},"items":[
 {"metadata":{"name":"jellyfin","namespace":"jellyfin"},"spec":{"template":{"spec":{"containers":[{"image":"jellyfin/jellyfin:12.2.0"}]}}}}]}`

const daemonsets = `{"metadata":{},"items":[
 {"metadata":{"name":"speaker","namespace":"metallb","labels":{"helm.toolkit.fluxcd.io/name":"metallb"}},"spec":{"template":{"spec":{"containers":[{"image":"quay.io/metallb/speaker:v0.15.2"}]}}}}]}`

const cronjobs = `{"metadata":{},"items":[
 {"metadata":{"name":"shoko-import","namespace":"shoko"},
  "spec":{"jobTemplate":{"spec":{"template":{"spec":{
    "initContainers":[{"image":"alpine:3.22"}],
    "containers":[{"image":"curlimages/curl:8.15.0"}]}}}}}}]}`

const helmreleases = `{"metadata":{},"items":[
 {"metadata":{"name":"traefik","namespace":"traefik"},
  "spec":{"chart":{"spec":{"chart":"traefik","version":"39.x","sourceRef":{"kind":"HelmRepository","name":"traefik","namespace":"flux-system"}}}},
  "status":{"history":[{"chartVersion":"39.0.2","appVersion":"v3.6.1"},{"chartVersion":"39.0.1","appVersion":"v3.6.0"}]}},
 {"metadata":{"name":"metallb","namespace":"metallb"},
  "spec":{"chart":{"spec":{"chart":"metallb","version":"0.15.x","sourceRef":{"kind":"HelmRepository","name":"metallb"}}}},
  "status":{}}]}`

const helmrepos = `{"metadata":{},"items":[
 {"metadata":{"name":"traefik","namespace":"flux-system"},"spec":{"url":"https://traefik.github.io/charts"}},
 {"metadata":{"name":"cnpg","namespace":"cnpg-system"},"spec":{"url":"oci://ghcr.io/cloudnative-pg/charts","type":"oci"}}]}`

const fluxDeployments = `{"metadata":{},"items":[
 {"metadata":{"name":"odd","namespace":"flux-system","labels":{"foo":"bar"}}},
 {"metadata":{"name":"source-controller","namespace":"flux-system","labels":{"app.kubernetes.io/version":"v2.8.8"}}},
 {"metadata":{"name":"helm-controller","namespace":"flux-system","labels":{"app.kubernetes.io/version":"v9.9.9"}}}]}`

func fixtureHandler(t *testing.T, token string, hits *[]string) http.Handler {
	routes := map[string]string{
		"/apis/apps/v1/deployments":                          deployments,
		"/apis/apps/v1/statefulsets":                         statefulsets,
		"/apis/apps/v1/daemonsets":                           daemonsets,
		"/apis/batch/v1/cronjobs":                            cronjobs,
		"/apis/helm.toolkit.fluxcd.io/v2/helmreleases":       helmreleases,
		"/apis/source.toolkit.fluxcd.io/v1/helmrepositories": helmrepos,
		"/apis/apps/v1/namespaces/flux-system/deployments":   fluxDeployments,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-GET method %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); token != "" && got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "version-monitor" {
			t.Errorf("User-Agent = %q", got)
		}
		if r.URL.Query().Get("limit") != "500" {
			t.Errorf("limit missing on %s", r.URL)
		}
		if hits != nil {
			*hits = append(*hits, r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/nodes" {
			if r.URL.Query().Get("continue") == "abc" {
				fmt.Fprint(w, nodesPage2)
			} else {
				fmt.Fprint(w, nodesPage1)
			}
			return
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	})
}

func TestInventory(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(fixtureHandler(t, "s3cret", &hits))
	defer srv.Close()
	c := New(srv.URL+"/", srv.Client(), "s3cret")

	inv, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	wantNodes := []Node{
		{"cp-1", "v1.35.10", "Talos (v1.13.12)"},
		{"w-1", "v1.35.9", "Talos (v1.13.11)"},
	}
	if !reflect.DeepEqual(inv.Nodes, wantNodes) {
		t.Errorf("nodes = %+v", inv.Nodes)
	}

	wantWL := []Workload{
		{"Deployment", "monitoring", "grafana", "kube-prometheus-stack",
			[]string{"grafana/grafana:12.0.0", "quay.io/kiwigrid/k8s-sidecar:1.30.0", "busybox:1.37", "busybox:1.37"}},
		{"Deployment", "radarr", "radarr", "", []string{"lscr.io/linuxserver/radarr:6.1.1.10360-ls303"}},
		{"StatefulSet", "jellyfin", "jellyfin", "", []string{"jellyfin/jellyfin:12.2.0"}},
		{"DaemonSet", "metallb", "speaker", "metallb", []string{"quay.io/metallb/speaker:v0.15.2"}},
		{"CronJob", "shoko", "shoko-import", "", []string{"curlimages/curl:8.15.0", "alpine:3.22"}},
	}
	if !reflect.DeepEqual(inv.Workloads, wantWL) {
		t.Errorf("workloads = %+v", inv.Workloads)
	}

	wantHR := []HelmRelease{
		{"traefik", "traefik", "traefik", "39.x", "39.0.2", "v3.6.1", "HelmRepository", "traefik", "flux-system"},
		{"metallb", "metallb", "metallb", "0.15.x", "", "", "HelmRepository", "metallb", "metallb"},
	}
	// Field order of HelmRelease is Namespace, Name, ...; build explicitly.
	wantHR = []HelmRelease{
		{Namespace: "traefik", Name: "traefik", Chart: "traefik", SpecVersion: "39.x", ChartVersion: "39.0.2", AppVersion: "v3.6.1",
			SourceKind: "HelmRepository", SourceName: "traefik", SourceNamespace: "flux-system"},
		{Namespace: "metallb", Name: "metallb", Chart: "metallb", SpecVersion: "0.15.x",
			SourceKind: "HelmRepository", SourceName: "metallb", SourceNamespace: "metallb"},
	}
	if !reflect.DeepEqual(inv.HelmReleases, wantHR) {
		t.Errorf("helmreleases = %+v", inv.HelmReleases)
	}

	wantRepos := []HelmRepository{
		{"flux-system", "traefik", "https://traefik.github.io/charts", ""},
		{"cnpg-system", "cnpg", "oci://ghcr.io/cloudnative-pg/charts", "oci"},
	}
	if !reflect.DeepEqual(inv.HelmRepositories, wantRepos) {
		t.Errorf("helmrepositories = %+v", inv.HelmRepositories)
	}

	if inv.FluxVersion != "v2.8.8" {
		t.Errorf("FluxVersion = %q", inv.FluxVersion)
	}

	// Pagination: second node page requested.
	var sawCont bool
	for _, h := range hits {
		if strings.HasPrefix(h, "/api/v1/nodes") && strings.Contains(h, "continue=abc") {
			sawCont = true
		}
	}
	if !sawCont {
		t.Errorf("continue request not seen: %v", hits)
	}
}

func TestInventoryFluxVersionMissing(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		inner := fixtureHandler(t, "", nil)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "flux-system") {
				http.Error(w, "nope", status)
				return
			}
			inner.ServeHTTP(w, r)
		}))
		inv, err := New(srv.URL, srv.Client(), "").Inventory(context.Background())
		srv.Close()
		if err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if inv.FluxVersion != "" {
			t.Errorf("FluxVersion = %q", inv.FluxVersion)
		}
	}
}

func TestInventoryError(t *testing.T) {
	inner := fixtureHandler(t, "", nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/batch/v1/cronjobs" {
			http.Error(w, strings.Repeat("x", 500), http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	_, err := New(srv.URL, srv.Client(), "").Inventory(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "GET /apis/batch/v1/cronjobs") || !strings.Contains(msg, "500") {
		t.Errorf("error = %v", msg)
	}
	if strings.Count(msg, "x") > 200 {
		t.Errorf("body not truncated: %d x", strings.Count(msg, "x"))
	}
}

func TestInventoryContextCancelled(t *testing.T) {
	srv := httptest.NewServer(fixtureHandler(t, "", nil))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(srv.URL, srv.Client(), "").Inventory(ctx); err == nil {
		t.Fatal("expected error")
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func caPEM(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func TestNewFromKubeconfig(t *testing.T) {
	srv := httptest.NewTLSServer(fixtureHandler(t, "tok-a", nil))
	defer srv.Close()
	dir := t.TempDir()
	caB64 := base64.StdEncoding.EncodeToString(caPEM(srv))

	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: a
clusters:
- name: ca
  cluster: {server: %s, certificate-authority-data: %s}
- name: cb
  cluster: {server: https://other.invalid:6443, insecure-skip-tls-verify: true}
users:
- name: ua
  user: {token: tok-a}
- name: ub
  user: {token-file: tokfile}
contexts:
- name: a
  context: {cluster: ca, user: ua}
- name: b
  context: {cluster: cb, user: ub}
`, srv.URL, caB64)
	path := writeFile(t, dir, "kubeconfig", cfg)
	writeFile(t, dir, "tokfile", "tok-b\n")

	c, err := NewFromKubeconfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.base != srv.URL || c.token != "tok-a" {
		t.Errorf("base=%q token=%q", c.base, c.token)
	}
	// Connectivity through TLS verification using the embedded CA.
	inv, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Nodes) != 2 {
		t.Errorf("nodes = %d", len(inv.Nodes))
	}

	cb, err := NewFromKubeconfig(path, "b")
	if err != nil {
		t.Fatal(err)
	}
	if cb.base != "https://other.invalid:6443" || cb.token != "tok-b" {
		t.Errorf("context b: base=%q token=%q", cb.base, cb.token)
	}

	if _, err := NewFromKubeconfig(path, "nope"); err == nil {
		t.Error("expected error for unknown context")
	}
}

func TestNewFromKubeconfigCAFileRelative(t *testing.T) {
	srv := httptest.NewTLSServer(fixtureHandler(t, "t", nil))
	defer srv.Close()
	dir := t.TempDir()
	writeFile(t, dir, "ca.pem", string(caPEM(srv)))
	path := writeFile(t, dir, "kc", fmt.Sprintf(`current-context: x
clusters: [{name: c, cluster: {server: "%s", certificate-authority: ca.pem}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
`, srv.URL))
	c, err := NewFromKubeconfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewFromKubeconfigUnsupported(t *testing.T) {
	dir := t.TempDir()
	for name, user := range map[string]string{
		"exec":          "exec: {command: aws, apiVersion: client.authentication.k8s.io/v1}",
		"auth-provider": "auth-provider: {name: gcp}",
		"basic":         "username: admin\n    password: pw",
	} {
		path := writeFile(t, dir, name, fmt.Sprintf(`current-context: x
clusters: [{name: c, cluster: {server: "https://k.invalid", insecure-skip-tls-verify: true}}]
users:
- name: u
  user:
    %s
contexts: [{name: x, context: {cluster: c, user: u}}]
`, user))
		_, err := NewFromKubeconfig(path, "")
		if err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestNewInCluster(t *testing.T) {
	srv := httptest.NewTLSServer(fixtureHandler(t, "sa-token", nil))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))

	old := saDir
	defer func() { saDir = old }()
	saDir = t.TempDir()

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := NewInCluster(); err == nil || !strings.Contains(err.Error(), "KUBERNETES_SERVICE_HOST") {
		t.Errorf("missing env: err = %v", err)
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	if _, err := NewInCluster(); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("missing token: err = %v", err)
	}

	writeFile(t, saDir, "token", "sa-token\n")
	if _, err := NewInCluster(); err == nil || !strings.Contains(err.Error(), "CA") {
		t.Errorf("missing CA: err = %v", err)
	}

	writeFile(t, saDir, "ca.crt", string(caPEM(srv)))
	c, err := NewInCluster()
	if err != nil {
		t.Fatal(err)
	}
	if c.token != "sa-token" {
		t.Errorf("token = %q", c.token)
	}
	inv, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Nodes) != 2 {
		t.Errorf("nodes = %d", len(inv.Nodes))
	}
}
