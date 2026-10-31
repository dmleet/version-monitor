// Package config loads and validates config.yaml and resolves per-component
// overrides.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dmleet/version-monitor/internal/model"
)

// Unit is the granularity at which release lines are counted.
type Unit string

const (
	UnitMinor Unit = "minor" // group by (major, minor)
	UnitMajor Unit = "major" // group by (major)
)

// Config is the top-level config.yaml schema.
type Config struct {
	// Interval between runs. Default 6h.
	Interval time.Duration `yaml:"interval"`
	Defaults Defaults      `yaml:"defaults"`
	// Platform sources for the non-workload components.
	Platform Platform `yaml:"platform"`
	// EOL lists products to look up on endoflife.date and where each
	// product's running version comes from.
	EOL []EOLEntry `yaml:"eol"`
	// Overrides keyed by a glob matched against the component (see Match).
	Overrides map[string]Override `yaml:"overrides"`
	// ExcludeNamespaces are never scanned for images (lines-behind or EOL).
	ExcludeNamespaces []string `yaml:"exclude_namespaces"`
}

// Defaults apply to every component unless overridden.
type Defaults struct {
	Unit           Unit `yaml:"unit"`
	MaxLinesBehind int  `yaml:"max_lines_behind"`
}

// Platform names the upstreams for Kubernetes, Talos and Flux.
type Platform struct {
	KubernetesProduct string `yaml:"kubernetes_endoflife"` // endoflife.date product, default "kubernetes"
	TalosGitHub       string `yaml:"talos_github"`         // default "siderolabs/talos"
	FluxGitHub        string `yaml:"flux_github"`          // default "fluxcd/flux2"
}

// EOLEntry maps an endoflife.date product to the place its version is read.
// Exactly one of Source, HelmRelease, Image must be set.
type EOLEntry struct {
	Product string `yaml:"product"`
	// Source: "kubernetes" (node kubelet version) or "flux".
	Source string `yaml:"source,omitempty"`
	// HelmRelease "namespace/name": uses .status.history[0].appVersion.
	HelmRelease string `yaml:"helmrelease,omitempty"`
	// Image: one or more normalised repositories whose running tags are the
	// product version (all workloads are scanned, chart-managed included).
	Image StringList `yaml:"image,omitempty"`
}

// Override adjusts how one or more components are checked.
type Override struct {
	Unit           Unit   `yaml:"unit,omitempty"`
	MaxLinesBehind *int   `yaml:"max_lines_behind,omitempty"`
	Pin            string `yaml:"pin,omitempty"`       // "major": only candidates sharing the current major
	Ignore         bool   `yaml:"ignore,omitempty"`    // skip the component entirely
	TagRegex       string `yaml:"tag_regex,omitempty"` // must define a named group `v`
	tagRE          *regexp.Regexp
}

// StringList accepts either a scalar or a sequence in YAML.
type StringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*s = StringList{n.Value}
		return nil
	case yaml.SequenceNode:
		var out []string
		if err := n.Decode(&out); err != nil {
			return err
		}
		*s = out
		return nil
	}
	return fmt.Errorf("line %d: expected string or list", n.Line)
}

// Resolved is the effective policy for one component after overrides.
type Resolved struct {
	Unit           Unit
	MaxLinesBehind int
	PinMajor       bool
	Ignore         bool
	TagRegex       *regexp.Regexp // nil when unset
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse validates config bytes.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Interval == 0 {
		c.Interval = 6 * time.Hour
	}
	if c.Interval < time.Minute {
		return nil, fmt.Errorf("config: interval %s is below the 1m minimum", c.Interval)
	}
	if c.Defaults.Unit == "" {
		c.Defaults.Unit = UnitMinor
	}
	if err := validUnit(c.Defaults.Unit); err != nil {
		return nil, fmt.Errorf("config: defaults.unit: %w", err)
	}
	if c.Defaults.MaxLinesBehind < 0 {
		return nil, fmt.Errorf("config: defaults.max_lines_behind must be >= 0")
	}
	if c.Platform.KubernetesProduct == "" {
		c.Platform.KubernetesProduct = "kubernetes"
	}
	if c.Platform.TalosGitHub == "" {
		c.Platform.TalosGitHub = "siderolabs/talos"
	}
	if c.Platform.FluxGitHub == "" {
		c.Platform.FluxGitHub = "fluxcd/flux2"
	}
	if c.ExcludeNamespaces == nil {
		c.ExcludeNamespaces = []string{"kube-system", "flux-system"}
	}
	for i, e := range c.EOL {
		n := 0
		if e.Source != "" {
			n++
			if e.Source != "kubernetes" && e.Source != "flux" {
				return nil, fmt.Errorf("config: eol[%d] (%s): source must be \"kubernetes\" or \"flux\"", i, e.Product)
			}
		}
		if e.HelmRelease != "" {
			n++
			if !strings.Contains(e.HelmRelease, "/") {
				return nil, fmt.Errorf("config: eol[%d] (%s): helmrelease must be \"namespace/name\"", i, e.Product)
			}
		}
		if len(e.Image) > 0 {
			n++
		}
		if e.Product == "" || n != 1 {
			return nil, fmt.Errorf("config: eol[%d]: needs product and exactly one of source, helmrelease, image", i)
		}
	}
	for k, o := range c.Overrides {
		if o.Unit != "" {
			if err := validUnit(o.Unit); err != nil {
				return nil, fmt.Errorf("config: overrides[%q].unit: %w", k, err)
			}
		}
		if o.Pin != "" && o.Pin != "major" {
			return nil, fmt.Errorf("config: overrides[%q].pin: only \"major\" is supported", k)
		}
		if o.MaxLinesBehind != nil && *o.MaxLinesBehind < 0 {
			return nil, fmt.Errorf("config: overrides[%q].max_lines_behind must be >= 0", k)
		}
		if o.TagRegex != "" {
			re, err := regexp.Compile(o.TagRegex)
			if err != nil {
				return nil, fmt.Errorf("config: overrides[%q].tag_regex: %w", k, err)
			}
			if re.SubexpIndex("v") < 0 {
				return nil, fmt.Errorf("config: overrides[%q].tag_regex must define a named group (?P<v>...)", k)
			}
			o.tagRE = re
			c.Overrides[k] = o
		}
	}
	return &c, nil
}

func validUnit(u Unit) error {
	switch u {
	case UnitMinor, UnitMajor:
		return nil
	}
	return fmt.Errorf("%q is not \"minor\" or \"major\"", u)
}

// Resolve returns the effective policy for a component. Override keys are
// globs (`*` matches any run of characters, including `/`) tested against,
// in order: the component's name, "namespace/name", and for images the
// repository. All matching overrides apply; when several set the same
// field the longest (most specific) key wins.
func (c *Config) Resolve(comp model.Component) Resolved {
	r := Resolved{Unit: c.Defaults.Unit, MaxLinesBehind: c.Defaults.MaxLinesBehind}
	type hit struct {
		key string
		o   Override
	}
	var hits []hit
	for k, o := range c.Overrides {
		if Match(k, comp) {
			hits = append(hits, hit{k, o})
		}
	}
	// Shortest first so longer keys overwrite.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && len(hits[j].key) < len(hits[j-1].key); j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	for _, h := range hits {
		if h.o.Unit != "" {
			r.Unit = h.o.Unit
		}
		if h.o.MaxLinesBehind != nil {
			r.MaxLinesBehind = *h.o.MaxLinesBehind
		}
		if h.o.Pin == "major" {
			r.PinMajor = true
		}
		if h.o.Ignore {
			r.Ignore = true
		}
		if h.o.tagRE != nil {
			r.TagRegex = h.o.tagRE
		}
	}
	return r
}

// Match reports whether an override key applies to a component.
func Match(key string, comp model.Component) bool {
	candidates := []string{comp.Name}
	if comp.Namespace != "" {
		candidates = append(candidates, comp.Namespace+"/"+comp.Name)
	}
	if comp.Kind == model.KindImage && comp.Source.Image != "" && comp.Source.Image != comp.Name {
		candidates = append(candidates, comp.Source.Image)
	}
	re := globRE(key)
	for _, s := range candidates {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

var globCache = map[string]*regexp.Regexp{}

func globRE(glob string) *regexp.Regexp {
	if re, ok := globCache[glob]; ok {
		return re
	}
	var sb strings.Builder
	sb.WriteString("^")
	for i, part := range strings.Split(glob, "*") {
		if i > 0 {
			sb.WriteString(".*")
		}
		sb.WriteString(regexp.QuoteMeta(part))
	}
	sb.WriteString("$")
	re := regexp.MustCompile(sb.String())
	globCache[glob] = re
	return re
}

// Excluded reports whether a namespace is excluded from image scanning.
func (c *Config) Excluded(namespace string) bool {
	for _, n := range c.ExcludeNamespaces {
		if n == namespace {
			return true
		}
	}
	return false
}
