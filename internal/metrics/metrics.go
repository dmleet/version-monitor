// Package metrics exposes the latest Snapshot as Prometheus metrics.
//
// The collector rebuilds its whole metric set from the snapshot on every
// scrape rather than updating long-lived GaugeVecs, so a component that
// was upgraded (new `current`/`latest` labels) or removed simply stops
// being reported. Nothing accumulates.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dmleet/version-monitor/internal/model"
)

const ns = "version_monitor"

var (
	descLines = prometheus.NewDesc(ns+"_lines_behind",
		"Release lines (major.minor, or major for major-unit components) published beyond the one running.",
		[]string{"kind", "namespace", "name", "current", "latest"}, nil)
	descPolicy = prometheus.NewDesc(ns+"_behind_policy",
		"1 when lines_behind exceeds the component's max_lines_behind, else 0.",
		[]string{"kind", "namespace", "name", "current", "latest"}, nil)
	descEOL = prometheus.NewDesc(ns+"_eol_days",
		"Days until the running release cycle reaches end-of-life (negative when past).",
		[]string{"product", "cycle", "kind", "namespace", "name"}, nil)
	descCheck = prometheus.NewDesc(ns+"_check_success",
		"1 if every lookup against this source succeeded in the last run.",
		[]string{"source"}, nil)
	descComponents = prometheus.NewDesc(ns+"_components",
		"Components seen in the last run, by kind and check status.",
		[]string{"kind", "status"}, nil)
	descLastRun = prometheus.NewDesc(ns+"_last_run_timestamp_seconds",
		"Unix time the last run finished.", nil, nil)
	descDuration = prometheus.NewDesc(ns+"_last_run_duration_seconds",
		"Wall-clock seconds the last run took.", nil, nil)
	descInfo = prometheus.NewDesc(ns+"_build_info",
		"Build information.", []string{"version"}, nil)
)

// Collector serves the most recent snapshot.
type Collector struct {
	version string
	mu      sync.RWMutex
	snap    *model.Snapshot
}

// New returns an empty collector; Set installs a snapshot.
func New(version string) *Collector { return &Collector{version: version} }

// Set replaces the served snapshot.
func (c *Collector) Set(s *model.Snapshot) {
	c.mu.Lock()
	c.snap = s
	c.mu.Unlock()
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descLines, descPolicy, descEOL, descCheck, descComponents, descLastRun, descDuration, descInfo} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(descInfo, prometheus.GaugeValue, 1, c.version)
	c.mu.RLock()
	s := c.snap
	c.mu.RUnlock()
	if s == nil {
		return
	}
	counts := map[[2]string]float64{}
	for _, r := range s.Results {
		counts[[2]string{string(r.Kind), string(r.Status)}]++
		if r.Status != model.StatusOK {
			continue
		}
		labels := []string{string(r.Kind), r.Namespace, r.Name, r.Current, r.Latest}
		ch <- prometheus.MustNewConstMetric(descLines, prometheus.GaugeValue, float64(r.LinesBehind), labels...)
		v := 0.0
		if r.BehindPolicy() {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(descPolicy, prometheus.GaugeValue, v, labels...)
	}
	for k, v := range counts {
		ch <- prometheus.MustNewConstMetric(descComponents, prometheus.GaugeValue, v, k[0], k[1])
	}
	eolSeen := map[[5]string]bool{}
	eolErrors := 0.0
	for _, e := range s.EOL {
		if e.Err != nil {
			eolErrors++
			continue
		}
		if !e.HasEOL {
			continue
		}
		key := [5]string{e.Product, e.Cycle, string(e.Kind), e.Namespace, e.Name}
		if eolSeen[key] {
			continue // a duplicate label set would fail the whole scrape
		}
		eolSeen[key] = true
		ch <- prometheus.MustNewConstMetric(descEOL, prometheus.GaugeValue, e.Days(s.FinishedAt),
			e.Product, e.Cycle, string(e.Kind), e.Namespace, e.Name)
	}
	ch <- prometheus.MustNewConstMetric(descComponents, prometheus.GaugeValue, eolErrors, "eol", "error")
	for src, ok := range s.CheckSuccess {
		v := 0.0
		if ok {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(descCheck, prometheus.GaugeValue, v, src)
	}
	ch <- prometheus.MustNewConstMetric(descLastRun, prometheus.GaugeValue, float64(s.FinishedAt.Unix()))
	ch <- prometheus.MustNewConstMetric(descDuration, prometheus.GaugeValue, s.FinishedAt.Sub(s.StartedAt).Seconds())
}
