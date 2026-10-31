// Package model holds the types shared between the cluster reader, the
// upstream fetchers, the policy engine and the metrics exporter.
package model

import "time"

// Kind classifies what a Component is, and therefore where its latest
// version comes from. It is exported as the `kind` metric label.
type Kind string

const (
	KindHelmRelease Kind = "helmrelease" // latest from the chart repo's index.yaml
	KindImage       Kind = "image"       // latest from the container registry's tag list
	KindKubernetes  Kind = "kubernetes"  // latest from endoflife.date cycles
	KindTalos       Kind = "talos"       // latest from GitHub releases
	KindFlux        Kind = "flux"        // latest from GitHub releases
)

// Component is one versioned thing running in the cluster.
type Component struct {
	Kind      Kind
	Namespace string // empty for cluster-scoped kinds
	// Name identifies the component within its kind: the HelmRelease name,
	// the normalised image repository (e.g. "lscr.io/linuxserver/radarr",
	// "redis" for docker.io/library/redis), or the kind itself for
	// platform components.
	Name string
	// Current is the raw version string as found in the cluster: a chart
	// version ("1.10.2"), an image tag ("6.1.1.10360-ls303"), a kubelet
	// version ("v1.35.10"), a Talos version ("v1.13.12") or the Flux
	// controller version label ("v2.8.8").
	Current string
	// AppVersion is the HelmRelease's .status.history[0].appVersion; used
	// only for EOL lookups keyed on a HelmRelease. Empty otherwise.
	AppVersion string
	// HelmManaged is true for workloads rendered by a HelmRelease (they
	// carry the helm.toolkit.fluxcd.io/name label). Such images are kept
	// for EOL lookups but excluded from lines-behind checks.
	HelmManaged bool

	// Source describes where to look up newer versions.
	Source Source
}

// Source names an upstream to query for a component's version list.
type Source struct {
	// HelmRepoName / HelmRepoURL / Chart for KindHelmRelease.
	HelmRepoName string
	HelmRepoURL  string
	Chart        string
	// Image (normalised repository, no tag/digest) for KindImage.
	Image string
	// GitHubRepo ("owner/name") for KindTalos / KindFlux.
	GitHubRepo string
	// EndOfLifeProduct for KindKubernetes.
	EndOfLifeProduct string
}

// ID returns a stable identifier used for de-duplication and for matching
// config overrides: "<kind>:<namespace>/<name>" or "<kind>:<name>".
func (c Component) ID() string {
	if c.Namespace == "" {
		return string(c.Kind) + ":" + c.Name
	}
	return string(c.Kind) + ":" + c.Namespace + "/" + c.Name
}

// SourceID returns the label value for version_monitor_check_success:
// "helm:<repo>", "registry:<host>", "endoflife", "github", "kubernetes".
func (c Component) SourceID() string {
	switch c.Kind {
	case KindHelmRelease:
		return "helm:" + c.Source.HelmRepoName
	case KindImage:
		return "registry:" + RegistryHost(c.Source.Image)
	case KindKubernetes:
		return "endoflife"
	case KindTalos, KindFlux:
		return "github"
	}
	return string(c.Kind)
}

// RegistryHost returns the registry hostname for a normalised image
// repository. Repositories without a host component (e.g. "redis",
// "grafana/grafana") live on Docker Hub.
func RegistryHost(repo string) string {
	for i := 0; i < len(repo); i++ {
		if repo[i] == '/' {
			first := repo[:i]
			for j := 0; j < len(first); j++ {
				if first[j] == '.' || first[j] == ':' {
					return first
				}
			}
			return "docker.io"
		}
	}
	return "docker.io"
}

// Status is the outcome of a version check for one component.
type Status string

const (
	StatusOK      Status = "ok"      // compared successfully
	StatusIgnored Status = "ignored" // excluded by an `ignore` override
	StatusSkipped Status = "skipped" // untracked tag (digest-only, sha-, latest, ...)
	StatusNoMatch Status = "nomatch" // no upstream version matched the current tag's shape
	StatusError   Status = "error"   // upstream fetch failed
)

// Result is the outcome of the lines-behind check for one component.
type Result struct {
	Component
	Status      Status
	Err         error
	Latest      string // newest version on the same line-unit grouping; empty if unknown
	LinesBehind int
	MaxLines    int  // the component's max_lines_behind threshold
	Pinned      bool // a `pin` override restricted the candidate set
}

// BehindPolicy reports whether the component violates its threshold.
func (r Result) BehindPolicy() bool {
	return r.Status == StatusOK && r.LinesBehind > r.MaxLines
}

// EOLResult is the outcome of an end-of-life lookup for one product
// instance. One product may yield several results (e.g. postgresql
// matched by two different images).
type EOLResult struct {
	Product string
	Cycle   string // matched endoflife.date cycle name, e.g. "1.35" or "17"
	Kind    Kind
	// Namespace/Name identify where the version was observed.
	Namespace string
	Name      string
	Version   string // the normalised version that was matched
	// HasEOL is false when the cycle has no EOL date (metric is omitted).
	HasEOL bool
	EOL    time.Time
	Err    error
}

// Days returns days until EOL at the given instant; negative when past.
func (e EOLResult) Days(now time.Time) float64 {
	return e.EOL.Sub(now).Hours() / 24
}

// Cycle is one release line as published by endoflife.date.
type Cycle struct {
	Name   string     // "1.35", "17", "3.7"
	Latest string     // latest patch release in the cycle, e.g. "1.35.10"
	EOL    *time.Time // nil when the cycle has no EOL date
	IsEOL  bool
}

// Snapshot is everything one run produced; the metrics collector serves
// the latest Snapshot.
type Snapshot struct {
	Results      []Result
	EOL          []EOLResult
	CheckSuccess map[string]bool // keyed by SourceID
	StartedAt    time.Time
	FinishedAt   time.Time
}
