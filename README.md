# version-monitor

A small Prometheus exporter that answers two questions about a Kubernetes
cluster: **is anything past end-of-life**, and **is anything further behind
upstream than our policy allows**. It reads the cluster through the API,
asks each upstream once, and serves the answer as metrics. Alerting and
dashboards are left to Prometheus, Alertmanager and Grafana.

## What it checks

| Kind | Current version from | Newer versions from |
| --- | --- | --- |
| Flux `HelmRelease` | `.status.history[0].chartVersion` | the `HelmRepository`'s `index.yaml` (HTTP repos; OCI is reported as an error) |
| Container image | tag on every `Deployment`, `StatefulSet`, `DaemonSet`, `CronJob` container and initContainer, and the image of every CloudNativePG `Cluster` (its pods have no workload controller) | the registry's tag list: Docker Hub, quay.io, and any OCI registry with the anonymous bearer flow (ghcr.io, lscr.io, nvcr.io, registry.k8s.io) |
| Kubernetes | lowest `kubeletVersion` across nodes | endoflife.date `kubernetes` cycles |
| Talos | lowest `Talos (vX.Y.Z)` in node `osImage` | GitHub releases of `siderolabs/talos` |
| Flux | `app.kubernetes.io/version` label on the `flux-system` controllers | GitHub releases of `fluxcd/flux2` |

Images rendered by a HelmRelease (they carry the `helm.toolkit.fluxcd.io/name`
label) are **not** checked for lines behind: the chart check covers them. They
*are* still scanned for end-of-life, because products like Grafana or Redis
only exist inside charts.

### Lines behind

Versions are compared by **shape**: every run of digits is replaced by `#`,
and only tags with the same shape as the running tag are candidates. That
drops pre-releases, variants and build tags without per-image rules
(`1.10.2` ≠ `1.11.0-rc.1`; `13.2.5-distroless` compares only with other
`-distroless` tags). Candidates are grouped into **release lines**,
`(major, minor)` by default or `(major)` for `unit: major`, and the metric is
the number of lines newer than the running one. `max_lines_behind: 1` is the
N−1 policy: one newer line is fine, two is a violation.

Tags that are not versions are skipped: `latest`-style floating tags, `sha-…`
and `<version>-<git sha>` build tags, digest-only references.

### End of life

Each configured product is resolved to the version actually running (a
HelmRelease's `appVersion`, an image tag, or a platform version), then
matched to the endoflife.date cycle whose name is the longest numeric prefix
of it (`16.11-alpine3.23` → cycle `16`; `v1.35.10` → `1.35`). The metric is
days until that cycle's EOL date; cycles without a date publish nothing.

## Metrics

| Metric | Labels | Value |
| --- | --- | --- |
| `version_monitor_lines_behind` | `kind, namespace, name, current, latest` | newer release lines |
| `version_monitor_behind_policy` | same | 1 when `lines_behind > max_lines_behind` |
| `version_monitor_eol_days` | `product, cycle, kind, namespace, name` | days until EOL, negative when past |
| `version_monitor_check_success` | `source` (`kubernetes`, `helm:<repo>`, `registry:<host>`, `endoflife`, `github`) | 1 if every lookup against that source succeeded |
| `version_monitor_components` | `kind, status` (`ok`, `ignored`, `skipped`, `nomatch`, `error`) | component count |
| `version_monitor_last_run_timestamp_seconds`, `_last_run_duration_seconds` | — | run bookkeeping |
| `version_monitor_build_info` | `version` | 1 |

The whole metric set is rebuilt from the last run on every scrape, so an
upgraded or removed component's old series disappear instead of lingering.

If the cluster read fails, the previous run's results stay published and only
`check_success{source="kubernetes"}` drops to 0.

## Configuration

```yaml
interval: 6h                                  # time between runs (min 1m)
defaults: {unit: minor, max_lines_behind: 1}
exclude_namespaces: [kube-system, flux-system]  # not scanned for images
platform:                                     # all optional, these are the defaults
  kubernetes_endoflife: kubernetes
  talos_github: siderolabs/talos
  flux_github: fluxcd/flux2
eol:                                          # endoflife.date product → where its version is read
  - {product: kubernetes,   source: kubernetes}
  - {product: flux,         source: flux}
  - {product: longhorn,     helmrelease: longhorn-system/longhorn}   # uses appVersion
  - {product: grafana,      image: grafana/grafana}                  # every workload, charts included
  - {product: postgresql,   image: [postgres, ghcr.io/cloudnative-pg/postgresql]}
overrides:                                    # glob → settings; longer keys win
  kube-prometheus-stack: {unit: major}
  tandoor-recipes/postgres: {unit: major, pin: major}   # pin: only candidates on the same major
  "lscr.io/linuxserver/*": {tag_regex: '^(?P<v>\d+\.\d+\.\d+\.\d+)-ls\d+$'}
  "dmleet/*": {ignore: true}
  ubuntu: {unit: major, max_lines_behind: 2}
```

Override keys are globs (`*` matches anything, including `/`) tested against
the component's name, `namespace/name`, and for images the repository as
written in the manifest (`lscr.io/...` stays `lscr.io/...`). Keys are not
kind-aware: `postgres` matches an image repository *and* a HelmRelease of
that name. When several keys match, the longest wins field by field.
`pin: major` with `unit: major` always yields 0 lines (useful to keep the
dashboard row while never alerting). Image names are
normalised: `docker.io/library/redis` is `redis`, `docker.io/grafana/grafana`
is `grafana/grafana`.

`tag_regex` must define a named group `v`; only tags matching the regex are
considered, and the running tag must match too or the component is skipped.

`github_releases: owner/repo` takes an image's candidates from that repo's
non-draft, non-prerelease release tags instead of the registry — one request
instead of paging a registry with thousands of tags (ghcr.io rate-limits
those). `{name}` is replaced by the image's last path segment.

## Running

```
version-monitor --config /etc/version-monitor/config.yaml     # in-cluster, serves :8080
version-monitor --dry-run --kubeconfig ~/.kube/config --config config.yaml
```

| Flag | Default | |
| --- | --- | --- |
| `--config` | `/etc/version-monitor/config.yaml` | |
| `--listen` | `:8080` | `/metrics`, `/healthz` (always 200), `/readyz` (200 after the first run) |
| `--kubeconfig`, `--context` | in-cluster | token, token-file and client-cert kubeconfigs; no exec plugins |
| `--dry-run` | | one run, results as a table, exit 1 if any source failed |
| `--concurrency` | 8 | parallel upstream fetches |
| `--run-timeout` | 10m | |
| `--log-format`, `--log-level` | text, info | |
| `--github-token-file` / `GITHUB_TOKEN` | | optional; lifts the anonymous 60/h GitHub limit |

The in-cluster ServiceAccount needs `get`/`list` on `nodes`, `deployments`,
`statefulsets`, `daemonsets`, `cronjobs`, `helmreleases.helm.toolkit.fluxcd.io`
and `helmrepositories.source.toolkit.fluxcd.io`. Nothing else, and no Secrets.

Outbound calls, all anonymous and read-only: the chart repositories, the
registries' tag APIs (never a pull), `endoflife.date`, `api.github.com`.
GET requests are retried twice on 429/5xx with `Retry-After` honoured.
Registry tag lists stop with an error (not a silent cut) past 30 pages;
bodies are bounded. A run against ~60 components takes a few seconds; the
chart indexes are decoded one at a time to cap memory (~80 MB for a 6.6 MB
index).

## Image

`dmleet/version-monitor` on Docker Hub, built by GitHub Actions from this
repo: a static binary on `scratch` with the CA bundle, running as `65534`.
Pin the **snapshot tag** `<version>-<sha>`; `<version>` and `latest` are
overwritten by rebuilds.

## Development

```
go test ./...
go run ./cmd/version-monitor --dry-run --kubeconfig ~/.kube/config --config ../flux/clusters/home/monitoring/version-monitor/config.yaml
```

`docs/reference/` holds the Python scratch scripts this was derived from and
the image inventory used to validate the first run.
