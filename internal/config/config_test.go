package config

import (
	"strings"
	"testing"
	"time"

	"github.com/dmleet/version-monitor/internal/model"
)

const sample = `
interval: 6h
defaults: {unit: minor, max_lines_behind: 1}
eol:
  - {product: kubernetes, source: kubernetes}
  - {product: flux, source: flux}
  - {product: longhorn, helmrelease: longhorn-system/longhorn}
  - {product: grafana, image: grafana/grafana}
  - {product: postgresql, image: [postgres, ghcr.io/cloudnative-pg/postgresql]}
overrides:
  kube-prometheus-stack: {unit: major}
  "tandoor-recipes/postgres": {unit: major, pin: major}
  "lscr.io/linuxserver/*": {tag_regex: '^(?P<v>\d+\.\d+\.\d+\.\d+)-ls\d+$', github_releases: "linuxserver/docker-{name}"}
  "lscr.io/linuxserver/sonarr": {max_lines_behind: 3}
  "dmleet/*": {ignore: true}
`

func TestParseAndResolve(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 6*time.Hour || c.Defaults.Unit != UnitMinor || c.Defaults.MaxLinesBehind != 1 {
		t.Errorf("defaults not parsed: %+v", c)
	}
	if c.Platform.TalosGitHub != "siderolabs/talos" || c.Platform.KubernetesProduct != "kubernetes" {
		t.Errorf("platform defaults missing: %+v", c.Platform)
	}
	if !c.Excluded("kube-system") || c.Excluded("monitoring") {
		t.Error("default exclude_namespaces wrong")
	}
	if got := c.EOL[4].Image; len(got) != 2 || got[1] != "ghcr.io/cloudnative-pg/postgresql" {
		t.Errorf("image list: %v", got)
	}
	if got := c.EOL[3].Image; len(got) != 1 || got[0] != "grafana/grafana" {
		t.Errorf("image scalar: %v", got)
	}

	kps := model.Component{Kind: model.KindHelmRelease, Namespace: "monitoring", Name: "kube-prometheus-stack"}
	if r := c.Resolve(kps); r.Unit != UnitMajor || r.MaxLinesBehind != 1 || r.Ignore {
		t.Errorf("kps resolve: %+v", r)
	}
	pg := model.Component{Kind: model.KindImage, Namespace: "tandoor-recipes", Name: "postgres", Source: model.Source{Image: "postgres"}}
	if r := c.Resolve(pg); r.Unit != UnitMajor || !r.PinMajor {
		t.Errorf("pinned postgres resolve: %+v", r)
	}
	otherPG := model.Component{Kind: model.KindImage, Namespace: "sure", Name: "postgres", Source: model.Source{Image: "postgres"}}
	if r := c.Resolve(otherPG); r.Unit != UnitMinor || r.PinMajor {
		t.Errorf("override leaked to another namespace: %+v", r)
	}
	radarr := model.Component{Kind: model.KindImage, Namespace: "radarr", Name: "lscr.io/linuxserver/radarr", Source: model.Source{Image: "lscr.io/linuxserver/radarr"}}
	if r := c.Resolve(radarr); r.TagRegex == nil || r.MaxLinesBehind != 1 || r.GitHubReleases != "linuxserver/docker-radarr" {
		t.Errorf("radarr resolve: %+v", r)
	}
	sonarr := model.Component{Kind: model.KindImage, Namespace: "sonarr", Name: "lscr.io/linuxserver/sonarr", Source: model.Source{Image: "lscr.io/linuxserver/sonarr"}}
	if r := c.Resolve(sonarr); r.TagRegex == nil || r.MaxLinesBehind != 3 {
		t.Errorf("sonarr should get both the glob regex and its own threshold: %+v", r)
	}
	beets := model.Component{Kind: model.KindImage, Namespace: "beets", Name: "dmleet/beets-hermes", Source: model.Source{Image: "dmleet/beets-hermes"}}
	if r := c.Resolve(beets); !r.Ignore {
		t.Errorf("dmleet glob should ignore: %+v", r)
	}
}

func TestParseErrors(t *testing.T) {
	bad := map[string]string{
		"unknown field":                   "defaults: {unit: minor}\nbogus: 1\n",
		"bad unit":                        "defaults: {unit: patch}\n",
		"bad pin":                         "overrides:\n  x: {pin: minor}\n",
		"regex without v":                 "overrides:\n  x: {tag_regex: '^\\d+$'}\n",
		"eol two sources":                 "eol:\n  - {product: x, source: flux, image: y}\n",
		"eol bad source":                  "eol:\n  - {product: x, source: nodes}\n",
		"eol bad helmrelease":             "eol:\n  - {product: x, helmrelease: longhorn}\n",
		"interval too short":              "interval: 10s\n",
		"github_releases without owner":   "overrides:\n  x: {github_releases: radarr}\n",
		"github_releases with path chars": "overrides:\n  x: {github_releases: \"a/b/../c\"}\n",
	}
	for name, src := range bad {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if !strings.HasPrefix(err.Error(), "config:") {
			t.Errorf("%s: error not prefixed: %v", name, err)
		}
	}
}

func TestResolveTieBreakIsDeterministic(t *testing.T) {
	src := "overrides:\n  \"a*\": {unit: major}\n  \"*b\": {unit: minor}\n"
	comp := model.Component{Kind: model.KindImage, Name: "ab", Source: model.Source{Image: "ab"}}
	for i := 0; i < 50; i++ {
		c, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		// equal length keys: "a*" < "*b" is false ('*' sorts before 'a'), so "*b" applies first and "a*" wins
		if r := c.Resolve(comp); r.Unit != UnitMajor {
			t.Fatalf("iteration %d: unit = %s, want major", i, r.Unit)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	img := model.Component{Kind: model.KindImage, Namespace: "ns", Name: "ghcr.io/foo/bar", Source: model.Source{Image: "ghcr.io/foo/bar"}}
	for glob, want := range map[string]bool{
		"ghcr.io/foo/bar": true, "ghcr.io/foo/*": true, "ghcr.io/*": true, "*": true,
		"ns/ghcr.io/foo/bar": true, "ghcr.io/foo": false, "foo/bar": false, "other/*": false,
	} {
		if got := Match(glob, img); got != want {
			t.Errorf("Match(%q) = %v, want %v", glob, got, want)
		}
	}
}
