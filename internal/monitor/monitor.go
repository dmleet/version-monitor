// Package monitor orchestrates one run: read the cluster, look up every
// upstream once, apply the policy, and produce a Snapshot.
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmleet/version-monitor/internal/cluster"
	"github.com/dmleet/version-monitor/internal/config"
	"github.com/dmleet/version-monitor/internal/helmindex"
	"github.com/dmleet/version-monitor/internal/model"
	"github.com/dmleet/version-monitor/internal/policy"
	"github.com/dmleet/version-monitor/internal/registry"
)

// Cluster reads the running inventory.
type Cluster interface {
	Inventory(ctx context.Context) (*cluster.Inventory, error)
}

// Registry lists image tags.
type Registry interface {
	Tags(ctx context.Context, repo string) ([]string, error)
}

// Helm fetches chart repository indexes.
type Helm interface {
	Index(ctx context.Context, repoURL string) (*helmindex.Index, error)
}

// EndOfLife fetches endoflife.date cycles.
type EndOfLife interface {
	Product(ctx context.Context, name string) ([]model.Cycle, error)
}

// GitHub lists stable release tags.
type GitHub interface {
	Releases(ctx context.Context, repo string) ([]string, error)
}

// Monitor holds the dependencies for a run.
type Monitor struct {
	Config      *config.Config
	Cluster     Cluster
	Registry    Registry
	Helm        Helm
	EndOfLife   EndOfLife
	GitHub      GitHub
	Concurrency int // parallel upstream fetches; default 8
	Log         *slog.Logger
	Now         func() time.Time
}

// ErrInventory wraps a cluster read failure so the caller can keep the
// previous snapshot's results while still reporting the failed check.
type ErrInventory struct{ Err error }

func (e *ErrInventory) Error() string { return "cluster inventory: " + e.Err.Error() }
func (e *ErrInventory) Unwrap() error { return e.Err }

var talosRE = regexp.MustCompile(`Talos \((v?\d+\.\d+\.\d+)\)`)

// Run performs one complete check. On an inventory failure it returns a
// Snapshot whose CheckSuccess marks "kubernetes" false and an *ErrInventory.
func (m *Monitor) Run(ctx context.Context) (*model.Snapshot, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	snap := &model.Snapshot{StartedAt: now(), CheckSuccess: map[string]bool{}}

	inv, err := m.Cluster.Inventory(ctx)
	if err != nil {
		snap.CheckSuccess["kubernetes"] = false
		snap.FinishedAt = now()
		return snap, &ErrInventory{err}
	}
	snap.CheckSuccess["kubernetes"] = true

	comps := m.components(inv, log)
	f := newFetchCache(m, log)

	// Decide what to fetch, then fetch everything once, in parallel.
	for _, c := range comps {
		if c.HelmManaged && c.Kind == model.KindImage {
			continue
		}
		res := m.Config.Resolve(c)
		if res.Ignore {
			continue
		}
		f.want(c)
	}
	observed, eolErrs := m.observe(comps)
	for p := range observed {
		f.wantProduct(p)
	}
	f.run(ctx, m.Concurrency)

	for _, c := range comps {
		if c.HelmManaged && c.Kind == model.KindImage {
			continue
		}
		snap.Results = append(snap.Results, m.check(c, f))
	}
	snap.EOL = append(eolErrs, m.eol(observed, f)...)

	for src, ok := range f.success {
		snap.CheckSuccess[src] = ok
	}
	for _, r := range snap.Results {
		if r.Err != nil {
			log.Warn("component check failed", "component", r.ID(), "current", r.Current, "err", r.Err)
		}
	}
	for _, e := range snap.EOL {
		if e.Err != nil {
			log.Warn("eol lookup failed", "product", e.Product, "kind", e.Kind, "namespace", e.Namespace, "name", e.Name, "version", e.Version, "err", e.Err)
		}
	}
	sort.Slice(snap.Results, func(i, j int) bool { return snap.Results[i].ID() < snap.Results[j].ID() })
	sort.Slice(snap.EOL, func(i, j int) bool {
		a, b := snap.EOL[i], snap.EOL[j]
		if a.Product != b.Product {
			return a.Product < b.Product
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	snap.FinishedAt = now()
	return snap, nil
}

// components turns the inventory into the list of things to check.
func (m *Monitor) components(inv *cluster.Inventory, log *slog.Logger) []model.Component {
	var out []model.Component

	repos := map[string]cluster.HelmRepository{}
	for _, r := range inv.HelmRepositories {
		repos[r.Namespace+"/"+r.Name] = r
	}
	for _, hr := range inv.HelmReleases {
		c := model.Component{
			Kind: model.KindHelmRelease, Namespace: hr.Namespace, Name: hr.Name,
			Current: hr.ChartVersion, AppVersion: hr.AppVersion,
			Source: model.Source{Chart: hr.Chart},
		}
		if c.Current == "" {
			c.Current = hr.SpecVersion
		}
		c.Source.ChartRef = hr.ChartRef
		if hr.SourceKind == "HelmRepository" {
			if r, ok := repos[hr.SourceNamespace+"/"+hr.SourceName]; ok {
				c.Source.HelmRepoName = r.Name
				c.Source.HelmRepoURL = r.URL
				if r.Type == "oci" {
					c.Source.HelmRepoURL = "" // unsupported; surfaces as an error result
				}
			}
		}
		out = append(out, c)
	}

	type imgKey struct{ ns, repo, tag string }
	seen := map[imgKey]int{}
	for _, w := range inv.Workloads {
		if m.Config.Excluded(w.Namespace) {
			continue
		}
		for _, img := range w.Images {
			repo, tag, _ := registry.Normalise(img)
			if tag == "" {
				continue // digest-only reference: no version to compare
			}
			k := imgKey{w.Namespace, repo, tag}
			if i, ok := seen[k]; ok {
				if w.HelmRelease == "" {
					out[i].HelmManaged = false
				}
				continue
			}
			seen[k] = len(out)
			out = append(out, model.Component{
				Kind: model.KindImage, Namespace: w.Namespace, Name: repo, Current: tag,
				HelmManaged: w.HelmRelease != "", Source: model.Source{Image: repo},
			})
		}
	}

	var kube, talos string
	for _, n := range inv.Nodes {
		if n.KubeletVersion != "" && (kube == "" || policyLess(n.KubeletVersion, kube)) {
			kube = n.KubeletVersion
		}
		if mm := talosRE.FindStringSubmatch(n.OSImage); mm != nil {
			if talos == "" || policyLess(mm[1], talos) {
				talos = mm[1]
			}
		}
	}
	if kube != "" {
		out = append(out, model.Component{Kind: model.KindKubernetes, Name: "kubernetes", Current: kube,
			Source: model.Source{EndOfLifeProduct: m.Config.Platform.KubernetesProduct}})
	}
	if talos != "" {
		out = append(out, model.Component{Kind: model.KindTalos, Name: "talos", Current: talos,
			Source: model.Source{GitHubRepo: m.Config.Platform.TalosGitHub}})
	}
	if inv.FluxVersion != "" {
		out = append(out, model.Component{Kind: model.KindFlux, Name: "flux", Current: inv.FluxVersion,
			Source: model.Source{GitHubRepo: m.Config.Platform.FluxGitHub}})
	} else {
		log.Warn("flux version not found in flux-system; skipping the flux check")
	}
	return out
}

// policyLess reports a < b by numeric components (lowest node wins).
func policyLess(a, b string) bool {
	return policy.Compare(policy.Parse(strings.TrimPrefix(a, "v")).Nums, policy.Parse(strings.TrimPrefix(b, "v")).Nums) < 0
}

// check applies the policy to one component using fetched candidates.
func (m *Monitor) check(c model.Component, f *fetchCache) model.Result {
	r := model.Result{Component: c}
	res := m.Config.Resolve(c)
	r.MaxLines = res.MaxLinesBehind
	if res.Ignore {
		r.Status = model.StatusIgnored
		return r
	}
	var cands []string
	var err error
	switch c.Kind {
	case model.KindHelmRelease:
		if c.Source.HelmRepoURL == "" {
			switch {
			case c.Source.ChartRef != "":
				r.Err = fmt.Errorf("spec.chartRef (%s) is not supported; only HelmRepository sources are checked", c.Source.ChartRef)
			case c.Source.HelmRepoName == "":
				r.Err = fmt.Errorf("sourceRef HelmRepository not found")
			default:
				r.Err = fmt.Errorf("HelmRepository %q is an OCI repository; only HTTP index repositories are checked", c.Source.HelmRepoName)
			}
			r.Status = model.StatusError
			return r
		}
		var idx *helmindex.Index
		idx, err = f.helm(c.Source.HelmRepoURL)
		if err == nil {
			entries, ok := idx.Entries(c.Source.Chart)
			if !ok {
				err = fmt.Errorf("chart %q not in index %s", c.Source.Chart, c.Source.HelmRepoURL)
			}
			for _, e := range entries {
				if policy.StableSemver(e.Version) {
					cands = append(cands, e.Version)
				}
			}
		}
	case model.KindImage:
		k, ok := imageFetch(c, res)
		if !ok {
			r.Status = model.StatusSkipped
			return r
		}
		if k.kind == "gh" {
			cands, err = f.releases(k.arg)
		} else {
			cands, err = f.tags(k.arg)
		}
	case model.KindKubernetes:
		var cycles []model.Cycle
		cycles, err = f.product(c.Source.EndOfLifeProduct)
		for _, cy := range cycles {
			if cy.Latest != "" {
				cands = append(cands, cy.Latest)
			}
		}
	case model.KindTalos, model.KindFlux:
		cands, err = f.releases(c.Source.GitHubRepo)
	}
	if err != nil {
		r.Status, r.Err = model.StatusError, err
		return r
	}
	out := policy.LinesBehind(policy.Input{
		Current: c.Current, Candidates: cands, Unit: res.Unit, PinMajor: res.PinMajor, TagRegex: res.TagRegex,
	})
	r.Status, r.Latest, r.LinesBehind, r.Pinned = out.Status, out.Latest, out.Lines, out.Pinned
	return r
}

// observation is one place a product's version was seen.
type observation struct {
	kind     model.Kind
	ns, name string
	version  string
}

// observe resolves every configured EOL product to the versions running.
// Products nothing runs are omitted (not fetched, not an error). Config
// entries that point at something missing produce an EOLResult with Err.
func (m *Monitor) observe(comps []model.Component) (map[string][]observation, []model.EOLResult) {
	observed := map[string][]observation{}
	var errs []model.EOLResult
	find := func(kind model.Kind) *model.Component {
		for i := range comps {
			if comps[i].Kind == kind {
				return &comps[i]
			}
		}
		return nil
	}
	for _, e := range m.Config.EOL {
		var obs []observation
		switch {
		case e.Source == "kubernetes":
			if c := find(model.KindKubernetes); c != nil {
				obs = append(obs, observation{c.Kind, "", c.Name, c.Current})
			}
		case e.Source == "flux":
			if c := find(model.KindFlux); c != nil {
				obs = append(obs, observation{c.Kind, "", c.Name, c.Current})
			}
		case e.HelmRelease != "":
			ns, name, _ := strings.Cut(e.HelmRelease, "/")
			found := false
			for _, c := range comps {
				if c.Kind == model.KindHelmRelease && c.Namespace == ns && c.Name == name {
					found = true
					if c.AppVersion == "" {
						errs = append(errs, model.EOLResult{Product: e.Product, Kind: c.Kind, Namespace: ns, Name: name,
							Err: fmt.Errorf("HelmRelease %s has no appVersion in status.history", e.HelmRelease)})
						continue
					}
					obs = append(obs, observation{c.Kind, ns, name, c.AppVersion})
				}
			}
			if !found {
				errs = append(errs, model.EOLResult{Product: e.Product, Kind: model.KindHelmRelease, Namespace: ns, Name: name,
					Err: fmt.Errorf("HelmRelease %s not found", e.HelmRelease)})
			}
		case len(e.Image) > 0:
			want := map[string]bool{}
			for _, img := range e.Image {
				r, _, _ := registry.Normalise(img)
				want[r] = true
			}
			for _, c := range comps {
				if c.Kind == model.KindImage && want[c.Source.Image] {
					obs = append(obs, observation{c.Kind, c.Namespace, c.Name, c.Current})
				}
			}
		}
		if len(obs) > 0 {
			observed[e.Product] = append(observed[e.Product], obs...)
		}
	}
	return observed, errs
}

// eol matches every observed version to its product's cycles.
func (m *Monitor) eol(observed map[string][]observation, f *fetchCache) []model.EOLResult {
	var out []model.EOLResult
	// One series per (product, cycle, kind, namespace, name): the same repo
	// at two tags in one namespace that land in the same cycle would
	// otherwise emit duplicate label sets and fail the whole scrape.
	seen := map[string]bool{}
	for product, obs := range observed {
		cycles, err := f.product(product)
		for _, o := range obs {
			r := model.EOLResult{Product: product, Kind: o.kind, Namespace: o.ns, Name: o.name, Version: policy.NormaliseVersion(o.version)}
			if err != nil {
				r.Err = err
				out = append(out, r)
				continue
			}
			cy, ok := policy.MatchCycle(r.Version, cycles)
			if !ok {
				r.Err = fmt.Errorf("no %s cycle matches version %q", product, r.Version)
				out = append(out, r)
				continue
			}
			r.Cycle = cy.Name
			key := strings.Join([]string{product, r.Cycle, string(r.Kind), r.Namespace, r.Name}, "|")
			if seen[key] {
				continue
			}
			seen[key] = true
			if cy.EOL != nil {
				r.HasEOL, r.EOL = true, *cy.EOL
			}
			out = append(out, r)
		}
	}
	return out
}

// fetchCache fetches each upstream key once and records per-source success.
type fetchCache struct {
	m       *Monitor
	log     *slog.Logger
	mu      sync.Mutex
	keys    map[string]fetchKey
	tagsR   map[string]fetched[[]string]
	helmR   map[string]fetched[*helmindex.Index]
	eolR    map[string]fetched[[]model.Cycle]
	ghR     map[string]fetched[[]string]
	success map[string]bool
}

type fetchKey struct {
	kind   string // "tags", "helm", "eol", "gh"
	arg    string
	source string // SourceID label
}

type fetched[T any] struct {
	v   T
	err error
}

func newFetchCache(m *Monitor, log *slog.Logger) *fetchCache {
	return &fetchCache{m: m, log: log, keys: map[string]fetchKey{},
		tagsR: map[string]fetched[[]string]{}, helmR: map[string]fetched[*helmindex.Index]{},
		eolR: map[string]fetched[[]model.Cycle]{}, ghR: map[string]fetched[[]string]{}, success: map[string]bool{}}
}

func (f *fetchCache) want(c model.Component) {
	switch c.Kind {
	case model.KindHelmRelease:
		if c.Source.HelmRepoURL != "" {
			f.keys["helm:"+c.Source.HelmRepoURL] = fetchKey{"helm", c.Source.HelmRepoURL, c.SourceID()}
		}
	case model.KindImage:
		if k, ok := imageFetch(c, f.m.Config.Resolve(c)); ok {
			f.keys[k.kind+":"+k.arg] = k
		}
	case model.KindKubernetes:
		f.wantProduct(c.Source.EndOfLifeProduct)
	case model.KindTalos, model.KindFlux:
		f.keys["gh:"+c.Source.GitHubRepo] = fetchKey{"gh", c.Source.GitHubRepo, "github"}
	}
}

// imageFetch decides where an image's candidates come from — the single
// place for that decision, used both when planning fetches and when
// checking. ok is false when the tag is not a version we can compare.
func imageFetch(c model.Component, res config.Resolved) (fetchKey, bool) {
	if !policy.Trackable(c.Current) && res.TagRegex == nil {
		return fetchKey{}, false
	}
	if res.GitHubReleases != "" {
		return fetchKey{"gh", res.GitHubReleases, "github"}, true
	}
	return fetchKey{"tags", c.Source.Image, c.SourceID()}, true
}

func (f *fetchCache) wantProduct(p string) {
	f.keys["eol:"+p] = fetchKey{"eol", p, "endoflife"}
}

func (f *fetchCache) run(ctx context.Context, concurrency int) {
	if concurrency <= 0 {
		concurrency = 8
	}
	for _, k := range f.keys {
		if _, ok := f.success[k.source]; !ok {
			f.success[k.source] = true
		}
	}
	sem := make(chan struct{}, concurrency)
	// Chart indexes are decoded whole by yaml.v3 (~12x the file size in
	// heap; prometheus-community's is 6.6 MB). One at a time keeps the
	// peak bounded regardless of how many repos there are.
	helmSem := make(chan struct{}, 1)
	var wg sync.WaitGroup
	for _, k := range f.keys {
		wg.Add(1)
		go func(k fetchKey) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if k.kind == "helm" {
				helmSem <- struct{}{}
				defer func() { <-helmSem }()
			}
			start := time.Now()
			var err error
			switch k.kind {
			case "tags":
				v, e := f.m.Registry.Tags(ctx, k.arg)
				err = e
				f.mu.Lock()
				f.tagsR[k.arg] = fetched[[]string]{v, e}
				f.mu.Unlock()
			case "helm":
				v, e := f.m.Helm.Index(ctx, k.arg)
				err = e
				f.mu.Lock()
				f.helmR[k.arg] = fetched[*helmindex.Index]{v, e}
				f.mu.Unlock()
			case "eol":
				v, e := f.m.EndOfLife.Product(ctx, k.arg)
				err = e
				f.mu.Lock()
				f.eolR[k.arg] = fetched[[]model.Cycle]{v, e}
				f.mu.Unlock()
			case "gh":
				v, e := f.m.GitHub.Releases(ctx, k.arg)
				err = e
				f.mu.Lock()
				f.ghR[k.arg] = fetched[[]string]{v, e}
				f.mu.Unlock()
			}
			f.mu.Lock()
			if err != nil {
				f.success[k.source] = false
				f.log.Warn("fetch failed", "kind", k.kind, "target", k.arg, "source", k.source, "err", err, "took", time.Since(start).Round(time.Millisecond))
			} else {
				f.log.Debug("fetched", "kind", k.kind, "target", k.arg, "took", time.Since(start).Round(time.Millisecond))
			}
			f.mu.Unlock()
		}(k)
	}
	wg.Wait()
}

func (f *fetchCache) tags(repo string) ([]string, error) {
	r, ok := f.tagsR[repo]
	if !ok {
		return nil, fmt.Errorf("tags for %s were not fetched", repo)
	}
	return r.v, r.err
}

func (f *fetchCache) helm(url string) (*helmindex.Index, error) {
	r, ok := f.helmR[url]
	if !ok {
		return nil, fmt.Errorf("index %s was not fetched", url)
	}
	return r.v, r.err
}

func (f *fetchCache) product(p string) ([]model.Cycle, error) {
	r, ok := f.eolR[p]
	if !ok {
		return nil, fmt.Errorf("endoflife product %s was not fetched", p)
	}
	return r.v, r.err
}

func (f *fetchCache) releases(repo string) ([]string, error) {
	r, ok := f.ghR[repo]
	if !ok {
		return nil, fmt.Errorf("releases for %s were not fetched", repo)
	}
	return r.v, r.err
}
