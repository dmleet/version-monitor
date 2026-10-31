// Package policy turns raw version strings into comparable versions and
// implements the two checks: release lines behind, and end-of-life cycle
// matching. It has no I/O.
package policy

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dmleet/version-monitor/internal/config"
	"github.com/dmleet/version-monitor/internal/model"
)

// Version is a parsed version string.
type Version struct {
	Raw  string // the string that was parsed (after Extract)
	Nums []int  // every run of digits, in order
	// Shape is Raw with each digit run replaced by '#'. Two tags are
	// comparable only when their shapes are equal, which is what filters
	// pre-releases, variant suffixes and build-number tags without any
	// per-image knowledge: "1.10.2" ≠ "1.11.0-rc.1", "13.2.5-distroless"
	// = "13.3.0-distroless".
	Shape string
}

var digitRun = regexp.MustCompile(`\d+`)

// Parse splits a version string into its digit runs and shape.
func Parse(s string) Version {
	v := Version{Raw: s, Shape: digitRun.ReplaceAllString(s, "#")}
	for _, m := range digitRun.FindAllString(s, -1) {
		if len(m) > 18 {
			m = m[:18] // avoid int overflow on absurd build numbers; ordering is preserved well enough
		}
		n, _ := strconv.Atoi(m)
		v.Nums = append(v.Nums, n)
	}
	return v
}

// Extract returns the comparable part of a tag. With a tag_regex the named
// group `v` is returned and ok is false when the regex doesn't match. With
// no regex, a leading "v" is stripped and ok is true.
func Extract(tag string, re *regexp.Regexp) (string, bool) {
	if re != nil {
		m := re.FindStringSubmatch(tag)
		if m == nil {
			return "", false
		}
		return m[re.SubexpIndex("v")], true
	}
	return strings.TrimPrefix(tag, "v"), true
}

var floatingTags = map[string]bool{
	"latest": true, "stable": true, "main": true, "master": true, "edge": true,
	"nightly": true, "dev": true, "develop": true, "unstable": true, "rolling": true,
}

// Trackable reports whether a tag names a version we can compare: not a
// floating tag, not a git-sha build tag, and containing at least one
// number. Tags whose trailing run of hex characters is 7+ long and
// contains a letter (e.g. "2.14.0-8fad9d3", "sha-e36cbfb", "e297ba3") are
// build snapshots, not versions.
func Trackable(tag string) bool {
	if tag == "" || floatingTags[tag] || strings.HasPrefix(tag, "sha-") {
		return false
	}
	if !digitRun.MatchString(tag) {
		return false
	}
	// trailing hex run
	i := len(tag)
	for i > 0 && isHex(tag[i-1]) {
		i--
	}
	run := tag[i:]
	if len(run) >= 7 && strings.ContainsAny(run, "abcdef") {
		return false
	}
	return true
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}

// Line returns the release-line key of a version at the given unit:
// [major] or [major, minor]. A version with a single number has minor 0.
func Line(v Version, unit config.Unit) []int {
	if len(v.Nums) == 0 {
		return nil
	}
	if unit == config.UnitMajor {
		return []int{v.Nums[0]}
	}
	if len(v.Nums) == 1 {
		return []int{v.Nums[0], 0}
	}
	return []int{v.Nums[0], v.Nums[1]}
}

// cmp compares two int slices lexicographically; a shorter prefix is less.
func cmp(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// Input to LinesBehind.
type Input struct {
	Current    string
	Candidates []string // upstream versions/tags, any order, raw
	Unit       config.Unit
	PinMajor   bool
	TagRegex   *regexp.Regexp
}

// Outcome of LinesBehind.
type Outcome struct {
	Status model.Status // StatusOK, StatusSkipped or StatusNoMatch
	Latest string       // raw candidate: newest on the newest line (or current if nothing newer)
	Lines  int          // distinct lines newer than the current one
	Pinned bool         // PinMajor dropped at least one candidate
}

// LinesBehind counts how many release lines exist beyond the current one.
func LinesBehind(in Input) Outcome {
	if in.TagRegex == nil && !Trackable(in.Current) {
		return Outcome{Status: model.StatusSkipped}
	}
	curRaw, ok := Extract(in.Current, in.TagRegex)
	if !ok {
		return Outcome{Status: model.StatusSkipped}
	}
	cur := Parse(curRaw)
	if len(cur.Nums) == 0 {
		return Outcome{Status: model.StatusSkipped}
	}
	curLine := Line(cur, in.Unit)

	type cand struct {
		raw string
		v   Version
	}
	var cands []cand
	pinned := false
	for _, raw := range in.Candidates {
		s, ok := Extract(raw, in.TagRegex)
		if !ok {
			continue
		}
		v := Parse(s)
		if v.Shape != cur.Shape || len(v.Nums) == 0 {
			continue
		}
		if in.PinMajor && v.Nums[0] != cur.Nums[0] {
			pinned = true
			continue
		}
		cands = append(cands, cand{raw, v})
	}
	if len(cands) == 0 {
		return Outcome{Status: model.StatusNoMatch, Pinned: pinned}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cmp(cands[i].v.Nums, cands[j].v.Nums) < 0 })

	newer := map[string]bool{}
	var newest []int
	for _, c := range cands {
		l := Line(c.v, in.Unit)
		if cmp(l, curLine) > 0 {
			newer[lineKey(l)] = true
		}
		if cmp(l, newest) >= 0 {
			newest = l
		}
	}
	best := cands[len(cands)-1]
	latest := best.raw
	if cmp(best.v.Nums, cur.Nums) <= 0 {
		latest = in.Current
	}
	return Outcome{Status: model.StatusOK, Latest: latest, Lines: len(newer), Pinned: pinned}
}

func lineKey(l []int) string {
	parts := make([]string, len(l))
	for i, n := range l {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// StableSemver reports whether a chart version is a plain X.Y.Z release
// (after stripping a leading "v"). Chart indexes list pre-releases that
// shape matching already excludes; this is a belt-and-braces filter for
// chart versions, which are always semver.
var stableSemver = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// StableSemver reports whether s is a plain X.Y.Z (optionally v-prefixed).
func StableSemver(s string) bool { return stableSemver.MatchString(s) }

// MatchCycle finds the endoflife.date cycle whose name is the longest
// numeric prefix of the running version. Cycle names differ per product
// ("17" for postgresql, "8.1" for valkey, "1.35" for kubernetes), and the
// running version carries suffixes we must ignore ("13.2.5-distroless",
// "16.11-alpine3.23", "v1.35.10"), so the comparison is on digit runs.
func MatchCycle(version string, cycles []model.Cycle) (model.Cycle, bool) {
	v := Parse(strings.TrimPrefix(version, "v"))
	var best model.Cycle
	bestLen := 0
	for _, c := range cycles {
		cn := Parse(c.Name).Nums
		if len(cn) == 0 || len(cn) > len(v.Nums) || len(cn) <= bestLen {
			continue
		}
		if cmp(cn, v.Nums[:len(cn)]) == 0 {
			best, bestLen = c, len(cn)
		}
	}
	return best, bestLen > 0
}

// NormaliseVersion strips a leading "v" and any "@digest" from a version
// string before cycle matching or display.
func NormaliseVersion(s string) string {
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "v")
}
