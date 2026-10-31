// Command version-monitor exports how far the cluster's components are
// behind upstream, and how close they are to end-of-life, as Prometheus
// metrics. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dmleet/version-monitor/internal/cluster"
	"github.com/dmleet/version-monitor/internal/config"
	"github.com/dmleet/version-monitor/internal/endoflife"
	"github.com/dmleet/version-monitor/internal/github"
	"github.com/dmleet/version-monitor/internal/helmindex"
	"github.com/dmleet/version-monitor/internal/httpx"
	"github.com/dmleet/version-monitor/internal/metrics"
	"github.com/dmleet/version-monitor/internal/model"
	"github.com/dmleet/version-monitor/internal/monitor"
	"github.com/dmleet/version-monitor/internal/registry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		configPath  = flag.String("config", "/etc/version-monitor/config.yaml", "path to config.yaml")
		listen      = flag.String("listen", ":8080", "address for /metrics, /healthz, /readyz")
		kubeconfig  = flag.String("kubeconfig", "", "kubeconfig path (default: in-cluster ServiceAccount)")
		kubeContext = flag.String("context", "", "kubeconfig context (default: current-context)")
		dryRun      = flag.Bool("dry-run", false, "run once, print the results as a table, exit 1 if any source failed")
		concurrency = flag.Int("concurrency", 8, "parallel upstream fetches")
		runTimeout  = flag.Duration("run-timeout", 10*time.Minute, "timeout for one complete run")
		logFormat   = flag.String("log-format", "text", "text or json")
		logLevel    = flag.String("log-level", "info", "debug, info, warn, error")
		githubToken = flag.String("github-token-file", "", "file holding a GitHub token (optional; raises the API rate limit). GITHUB_TOKEN env also works")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log := newLogger(*logFormat, *logLevel)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(2)
	}

	var cl *cluster.Client
	if *kubeconfig != "" {
		cl, err = cluster.NewFromKubeconfig(*kubeconfig, *kubeContext)
	} else {
		cl, err = cluster.NewInCluster()
	}
	if err != nil {
		log.Error("kubernetes client", "err", err)
		os.Exit(2)
	}

	// One retry on 429/5xx with Retry-After honoured: a single transient
	// error must not mark a source failed for a whole 6h interval.
	httpc := httpx.NewClient(60*time.Second, 2, 30*time.Second)
	gh := &github.Client{HTTP: httpc, BaseURL: github.DefaultBaseURL}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		gh.Token = strings.TrimSpace(tok)
	}
	if *githubToken != "" {
		b, err := os.ReadFile(*githubToken)
		if err != nil {
			log.Error("github token file", "err", err)
			os.Exit(2)
		}
		gh.Token = strings.TrimSpace(string(b))
	}
	m := &monitor.Monitor{
		Config:      cfg,
		Cluster:     cl,
		Registry:    registry.New(httpc),
		Helm:        &helmindex.Client{HTTP: httpc},
		EndOfLife:   &endoflife.Client{HTTP: httpc, BaseURL: endoflife.DefaultBaseURL},
		GitHub:      gh,
		Concurrency: *concurrency,
		Log:         log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *dryRun {
		rctx, cancel := context.WithTimeout(ctx, *runTimeout)
		defer cancel()
		snap, err := m.Run(rctx)
		if err != nil {
			log.Error("run", "err", err)
			os.Exit(1)
		}
		printTable(os.Stdout, snap)
		for _, ok := range snap.CheckSuccess {
			if !ok {
				os.Exit(1)
			}
		}
		return
	}

	coll := metrics.New(version)
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)
	var ready atomic.Bool

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "no run completed yet", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("listening", "addr", *listen, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	var last *model.Snapshot
	runOnce := func() {
		rctx, cancel := context.WithTimeout(ctx, *runTimeout)
		defer cancel()
		snap, err := m.Run(rctx)
		var invErr *monitor.ErrInventory
		if errors.As(err, &invErr) && last != nil {
			// Keep the previous results; only the health signal changes.
			merged := *last
			merged.CheckSuccess = make(map[string]bool, len(last.CheckSuccess)+1)
			for k, v := range last.CheckSuccess {
				merged.CheckSuccess[k] = v
			}
			merged.CheckSuccess["kubernetes"] = false
			merged.StartedAt, merged.FinishedAt = snap.StartedAt, snap.FinishedAt
			snap = &merged
		}
		if err != nil {
			log.Error("run", "err", err)
		}
		last = snap
		coll.Set(snap)
		ready.Store(true)
		log.Info("run complete", "results", len(snap.Results), "eol", len(snap.EOL),
			"behind_policy", countBehind(snap), "failed_sources", failedSources(snap),
			"took", snap.FinishedAt.Sub(snap.StartedAt).Round(time.Millisecond))
	}

	runOnce()
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
			return
		case <-t.C:
			runOnce()
		}
	}
}

func newLogger(format, level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func countBehind(s *model.Snapshot) int {
	n := 0
	for _, r := range s.Results {
		if r.BehindPolicy() {
			n++
		}
	}
	return n
}

func failedSources(s *model.Snapshot) []string {
	var out []string
	for src, ok := range s.CheckSuccess {
		if !ok {
			out = append(out, src)
		}
	}
	return out
}

// printTable renders a snapshot for --dry-run.
func printTable(w *os.File, s *model.Snapshot) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNAMESPACE\tNAME\tCURRENT\tLATEST\tLINES\tMAX\tSTATUS\tNOTE")
	for _, r := range s.Results {
		note := ""
		switch {
		case r.Err != nil:
			note = r.Err.Error()
		case r.BehindPolicy():
			note = "BEHIND POLICY"
		case r.Pinned:
			note = "pinned"
		}
		lines, max := "", ""
		if r.Status == model.StatusOK {
			lines, max = fmt.Sprint(r.LinesBehind), fmt.Sprint(r.MaxLines)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Kind, r.Namespace, r.Name, r.Current, r.Latest, lines, max, r.Status, note)
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "PRODUCT\tCYCLE\tVERSION\tKIND\tNAMESPACE\tNAME\tEOL\tDAYS\tNOTE")
	for _, e := range s.EOL {
		eol, days, note := "", "", ""
		switch {
		case e.Err != nil:
			note = e.Err.Error()
		case e.HasEOL:
			eol = e.EOL.Format("2006-01-02")
			days = fmt.Sprintf("%.0f", e.Days(s.FinishedAt))
		default:
			note = "no EOL date published"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Product, e.Cycle, e.Version, e.Kind, e.Namespace, e.Name, eol, days, note)
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "SOURCE\tOK")
	sources := make([]string, 0, len(s.CheckSuccess))
	for src := range s.CheckSuccess {
		sources = append(sources, src)
	}
	sort.Strings(sources)
	for _, src := range sources {
		fmt.Fprintf(tw, "%s\t%v\n", src, s.CheckSuccess[src])
	}
	fmt.Fprintf(tw, "\nrun took %s\n", s.FinishedAt.Sub(s.StartedAt).Round(time.Millisecond))
	tw.Flush()
}
