package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dmleet/version-monitor/internal/model"
)

func TestCollector(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	fin := start.Add(3 * time.Second)
	c := New("1.2.3")

	// Before any run: only build_info.
	if n := testutil.CollectAndCount(c); n != 1 {
		t.Fatalf("metrics before first snapshot = %d, want 1", n)
	}

	c.Set(&model.Snapshot{
		StartedAt: start, FinishedAt: fin,
		CheckSuccess: map[string]bool{"kubernetes": true, "registry:docker.io": false},
		Results: []model.Result{
			{Component: model.Component{Kind: model.KindHelmRelease, Namespace: "longhorn-system", Name: "longhorn", Current: "1.10.2"},
				Status: model.StatusOK, Latest: "1.13.0", LinesBehind: 3, MaxLines: 1},
			{Component: model.Component{Kind: model.KindTalos, Name: "talos", Current: "v1.13.12"},
				Status: model.StatusOK, Latest: "v1.14.5", LinesBehind: 1, MaxLines: 1},
			{Component: model.Component{Kind: model.KindImage, Namespace: "beets", Name: "dmleet/beets-hermes", Current: "x"}, Status: model.StatusIgnored},
			{Component: model.Component{Kind: model.KindImage, Namespace: "down", Name: "example/unreachable", Current: "1.0.0"}, Status: model.StatusError},
		},
		EOL: []model.EOLResult{
			{Product: "kubernetes", Cycle: "1.35", Kind: model.KindKubernetes, Name: "kubernetes", HasEOL: true, EOL: fin.Add(48 * time.Hour)},
			{Product: "flux", Cycle: "2.8", Kind: model.KindFlux, Name: "flux", HasEOL: false},
		},
	})

	want := `
# HELP version_monitor_behind_policy 1 when lines_behind exceeds the component's max_lines_behind, else 0.
# TYPE version_monitor_behind_policy gauge
version_monitor_behind_policy{current="1.10.2",kind="helmrelease",latest="1.13.0",name="longhorn",namespace="longhorn-system"} 1
version_monitor_behind_policy{current="v1.13.12",kind="talos",latest="v1.14.5",name="talos",namespace=""} 0
# HELP version_monitor_check_success 1 if every lookup against this source succeeded in the last run.
# TYPE version_monitor_check_success gauge
version_monitor_check_success{source="kubernetes"} 1
version_monitor_check_success{source="registry:docker.io"} 0
# HELP version_monitor_components Components seen in the last run, by kind and check status.
# TYPE version_monitor_components gauge
version_monitor_components{kind="eol",status="error"} 0
version_monitor_components{kind="helmrelease",status="ok"} 1
version_monitor_components{kind="image",status="error"} 1
version_monitor_components{kind="image",status="ignored"} 1
version_monitor_components{kind="talos",status="ok"} 1
# HELP version_monitor_eol_days Days until the running release cycle reaches end-of-life (negative when past).
# TYPE version_monitor_eol_days gauge
version_monitor_eol_days{cycle="1.35",kind="kubernetes",name="kubernetes",namespace="",product="kubernetes"} 2
# HELP version_monitor_last_run_duration_seconds Wall-clock seconds the last run took.
# TYPE version_monitor_last_run_duration_seconds gauge
version_monitor_last_run_duration_seconds 3
# HELP version_monitor_last_run_timestamp_seconds Unix time the last run finished.
# TYPE version_monitor_last_run_timestamp_seconds gauge
version_monitor_last_run_timestamp_seconds 1.793448003e+09
# HELP version_monitor_lines_behind Release lines (major.minor, or major for major-unit components) published beyond the one running.
# TYPE version_monitor_lines_behind gauge
version_monitor_lines_behind{current="1.10.2",kind="helmrelease",latest="1.13.0",name="longhorn",namespace="longhorn-system"} 3
version_monitor_lines_behind{current="v1.13.12",kind="talos",latest="v1.14.5",name="talos",namespace=""} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"version_monitor_lines_behind", "version_monitor_behind_policy", "version_monitor_eol_days",
		"version_monitor_check_success", "version_monitor_components",
		"version_monitor_last_run_timestamp_seconds", "version_monitor_last_run_duration_seconds"); err != nil {
		t.Error(err)
	}

	// A new snapshot without longhorn drops its series entirely.
	c.Set(&model.Snapshot{StartedAt: start, FinishedAt: fin, CheckSuccess: map[string]bool{}})
	if n := testutil.CollectAndCount(c, "version_monitor_lines_behind"); n != 0 {
		t.Errorf("stale series survived: %d", n)
	}
}
