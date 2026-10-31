package policy

import (
	"regexp"
	"testing"
	"time"

	"github.com/dmleet/version-monitor/internal/config"
	"github.com/dmleet/version-monitor/internal/model"
)

func TestParseShape(t *testing.T) {
	cases := map[string]string{
		"1.10.2":                             "#.#.#",
		"13.2.5-distroless":                  "#.#.#-distroless",
		"6.1.1.10360":                        "#.#.#.#",
		"12.2.20261005-225228":               "#.#.#-#",
		"16.11-alpine3.23":                   "#.#-alpine#.#",
		"1.11.0-rc.1":                        "#.#.#-rc.#",
		"17.11-202610190957-system-bookworm": "#.#-#-system-bookworm",
	}
	for in, want := range cases {
		if got := Parse(in).Shape; got != want {
			t.Errorf("Parse(%q).Shape = %q, want %q", in, got, want)
		}
	}
	if got := Parse("12.2.20261005-225228").Nums; len(got) != 4 || got[2] != 20261005 {
		t.Errorf("Nums = %v", got)
	}
}

func TestTrackable(t *testing.T) {
	yes := []string{"1.10.2", "v1.35.10", "13.2.5-distroless", "12.2.20261005-225228", "6.1.1.10360-ls303",
		"8.1.10-alpine", "2026.8.5", "1.2.3.1234567"}
	no := []string{"latest", "main", "sha-e36cbfb", "e297ba3", "2.14.0-8fad9d3", "2026.8.5-1689cb1b5", "", "alpine"}
	for _, s := range yes {
		if !Trackable(s) {
			t.Errorf("Trackable(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if Trackable(s) {
			t.Errorf("Trackable(%q) = true, want false", s)
		}
	}
}

func TestLinesBehind(t *testing.T) {
	lscr := regexp.MustCompile(`^(?P<v>\d+\.\d+\.\d+\.\d+)-ls\d+$`)
	cases := []struct {
		name   string
		in     Input
		status model.Status
		lines  int
		latest string
		pinned bool
	}{
		{
			name: "longhorn three lines behind (the 2026-10-31 example)",
			in: Input{Current: "1.10.2", Unit: config.UnitMinor,
				Candidates: []string{"1.9.0", "1.10.0", "1.10.1", "1.10.2", "1.11.0", "1.11.1", "1.11.2", "1.11.3", "1.12.0", "1.12.1", "1.13.0", "1.14.0-rc.1"}},
			status: model.StatusOK, lines: 3, latest: "1.13.0",
		},
		{
			name: "kubernetes from cycle latests, v-prefixed current",
			in: Input{Current: "v1.35.10", Unit: config.UnitMinor,
				Candidates: []string{"1.37.2", "1.36.6", "1.35.10", "1.34.13"}},
			status: model.StatusOK, lines: 2, latest: "1.37.2",
		},
		{
			name: "talos one line behind, prerelease excluded by shape",
			in: Input{Current: "v1.13.12", Unit: config.UnitMinor,
				Candidates: []string{"v1.15.0-alpha.2", "v1.14.5", "v1.13.12", "v1.14.4"}},
			status: model.StatusOK, lines: 1, latest: "v1.14.5",
		},
		{
			name: "flux two lines",
			in: Input{Current: "v2.8.8", Unit: config.UnitMinor,
				Candidates: []string{"v2.10.0", "v2.9.6", "v2.9.5", "v2.8.8"}},
			status: model.StatusOK, lines: 2, latest: "v2.10.0",
		},
		{
			name: "kps at major unit: patch behind is zero lines",
			in: Input{Current: "92.16.1", Unit: config.UnitMajor,
				Candidates: []string{"92.16.2", "92.16.1", "91.0.0"}},
			status: model.StatusOK, lines: 0, latest: "92.16.2",
		},
		{
			name: "traefik chart at major unit: 40 vs 42",
			in: Input{Current: "40.3.0", Unit: config.UnitMajor,
				Candidates: []string{"42.2.0", "42.1.0", "41.0.0", "40.3.0"}},
			status: model.StatusOK, lines: 2, latest: "42.2.0",
		},
		{
			name: "open-webui dev pre-releases dropped by shape",
			in: Input{Current: "15.2.1", Unit: config.UnitMajor,
				Candidates: []string{"17.5.1-dev.652.1", "17.0.0", "16.0.0", "15.2.1"}},
			status: model.StatusOK, lines: 2, latest: "17.0.0",
		},
		{
			name: "lscr tag_regex: nightly and arch tags excluded",
			in: Input{Current: "6.1.1.10360-ls303", Unit: config.UnitMinor, TagRegex: lscr,
				Candidates: []string{"amd64-6.2.0.10500-ls310", "nightly-6.3.0.1-ls1", "6.2.0.10500-ls310", "6.1.1.10360-ls303", "6.3.0.10600-ls320", "latest"}},
			status: model.StatusOK, lines: 2, latest: "6.3.0.10600-ls320",
		},
		{
			name: "lscr tag_regex not matching current (nightly) is skipped",
			in: Input{Current: "nightly-6.3.0.1-ls1", Unit: config.UnitMinor, TagRegex: lscr,
				Candidates: []string{"6.2.0.10500-ls310"}},
			status: model.StatusSkipped,
		},
		{
			name: "grafana distroless variant compared only with distroless",
			in: Input{Current: "13.2.5-distroless", Unit: config.UnitMinor,
				Candidates: []string{"13.3.0", "13.3.0-distroless", "13.2.5-distroless", "13.2.5", "13.4.0-ubuntu"}},
			status: model.StatusOK, lines: 1, latest: "13.3.0-distroless",
		},
		{
			name: "postgres pinned to major 16 at major unit",
			in: Input{Current: "16.11-alpine3.23", Unit: config.UnitMajor, PinMajor: true,
				Candidates: []string{"18.1-alpine3.23", "17.11-alpine3.23", "16.15-alpine3.23", "16.11-alpine3.23"}},
			status: model.StatusOK, lines: 0, latest: "16.15-alpine3.23", pinned: true,
		},
		{
			name: "cnpg postgres image at major unit: 17 vs 18 is one line",
			in: Input{Current: "17.11-202610190957-system-bookworm", Unit: config.UnitMajor,
				Candidates: []string{"18.1-202610190957-system-bookworm", "17.11-202610190957-system-bookworm", "17.11-202609010000-system-bookworm"}},
			status: model.StatusOK, lines: 1, latest: "18.1-202610190957-system-bookworm",
		},
		{
			name: "jellyfin build-dated tags count minor lines",
			in: Input{Current: "12.2.20261005-225228", Unit: config.UnitMinor,
				Candidates: []string{"12.4.20261030-010101", "12.3.20261020-121212", "12.2.20261005-225228", "10.11.0", "12.3.20261021-131313"}},
			status: model.StatusOK, lines: 2, latest: "12.4.20261030-010101",
		},
		{
			name: "valkey 8.1 vs 9.1",
			in: Input{Current: "8.1.10-alpine", Unit: config.UnitMinor,
				Candidates: []string{"9.1.3-alpine", "9.0.7-alpine", "8.1.11-alpine", "8.1.10-alpine", "9.1.3"}},
			status: model.StatusOK, lines: 2, latest: "9.1.3-alpine",
		},
		{
			name:   "newer than everything upstream reports current as latest",
			in:     Input{Current: "2.0.0", Unit: config.UnitMinor, Candidates: []string{"1.9.0", "1.8.0"}},
			status: model.StatusOK, lines: 0, latest: "2.0.0",
		},
		{
			name:   "no shape match",
			in:     Input{Current: "1.2.3", Unit: config.UnitMinor, Candidates: []string{"1.2.3-alpine", "v2"}},
			status: model.StatusNoMatch,
		},
		{
			name:   "floating tag skipped",
			in:     Input{Current: "latest", Unit: config.UnitMinor, Candidates: []string{"1.0.0"}},
			status: model.StatusSkipped,
		},
		{
			name:   "git sha tag skipped",
			in:     Input{Current: "2.14.0-8fad9d3", Unit: config.UnitMinor, Candidates: []string{"2.15.0-abcdef1"}},
			status: model.StatusSkipped,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LinesBehind(tc.in)
			if got.Status != tc.status {
				t.Fatalf("status = %s, want %s (%+v)", got.Status, tc.status, got)
			}
			if tc.status != model.StatusOK {
				return
			}
			if got.Lines != tc.lines || got.Latest != tc.latest || got.Pinned != tc.pinned {
				t.Errorf("got lines=%d latest=%q pinned=%v, want lines=%d latest=%q pinned=%v",
					got.Lines, got.Latest, got.Pinned, tc.lines, tc.latest, tc.pinned)
			}
		})
	}
}

func TestMatchCycle(t *testing.T) {
	d := func(s string) *time.Time {
		t, _ := time.Parse("2006-01-02", s)
		return &t
	}
	pg := []model.Cycle{{Name: "18", EOL: d("2030-11-14")}, {Name: "17", EOL: d("2029-11-08")}, {Name: "16", EOL: d("2028-11-09")}}
	k8s := []model.Cycle{{Name: "1.37"}, {Name: "1.36"}, {Name: "1.35", EOL: d("2027-02-28")}, {Name: "1.3"}}
	grafana := []model.Cycle{{Name: "13.3"}, {Name: "13.2", EOL: d("2027-05-18")}, {Name: "13"}}
	cases := []struct {
		version string
		cycles  []model.Cycle
		want    string
		ok      bool
	}{
		{"16.11-alpine3.23", pg, "16", true},
		{"17.11-202610190957-system-bookworm@sha256:abc", pg, "17", true},
		{"v1.35.10", k8s, "1.35", true},
		{"1.3.0", k8s, "1.3", true},
		{"13.2.5-distroless", grafana, "13.2", true},
		{"13.9.0", grafana, "13", true},
		{"8-alpine3.21", []model.Cycle{{Name: "8.0"}, {Name: "8.1"}, {Name: "8.3"}, {Name: "8"}}, "8", true},
		{"8-alpine3.21", []model.Cycle{{Name: "8.0"}, {Name: "8.1"}, {Name: "8.3"}}, "", false},
		{"v1.35.10", []model.Cycle{{Name: "1.3"}, {Name: "1.35"}}, "1.35", true},
		{"1.35.10", []model.Cycle{{Name: "1.3"}}, "", false},
		{"15.0", pg, "", false},
		{"", pg, "", false},
	}
	for _, tc := range cases {
		got, ok := MatchCycle(NormaliseVersion(tc.version), tc.cycles)
		if ok != tc.ok || got.Name != tc.want {
			t.Errorf("MatchCycle(%q) = (%q, %v), want (%q, %v)", tc.version, got.Name, ok, tc.want, tc.ok)
		}
	}
}

func TestStableSemver(t *testing.T) {
	for s, want := range map[string]bool{"1.2.3": true, "v1.2.3": true, "1.2.3-rc.1": false, "1.2": false, "17.5.1-dev.652.1": false} {
		if got := StableSemver(s); got != want {
			t.Errorf("StableSemver(%q) = %v, want %v", s, got, want)
		}
	}
}
