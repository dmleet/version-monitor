package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Node is a cluster node's reported versions.
type Node struct {
	Name           string
	KubeletVersion string // .status.nodeInfo.kubeletVersion, e.g. "v1.35.10"
	OSImage        string // .status.nodeInfo.osImage, e.g. "Talos (v1.13.12)"
}

// Workload is a pod-template-bearing object and the images it runs.
type Workload struct {
	Kind        string // "Deployment", "StatefulSet", "DaemonSet", "CronJob"
	Namespace   string
	Name        string
	HelmRelease string   // label helm.toolkit.fluxcd.io/name, "" if absent
	Images      []string // containers then initContainers, duplicates kept
}

// HelmRelease is a Flux HelmRelease and its installed chart version.
type HelmRelease struct {
	Namespace, Name string
	Chart           string
	SpecVersion     string
	ChartVersion    string // .status.history[0].chartVersion
	AppVersion      string // .status.history[0].appVersion
	SourceKind      string
	SourceName      string
	SourceNamespace string // defaults to the HelmRelease's namespace
}

// HelmRepository is a Flux HelmRepository source.
type HelmRepository struct {
	Namespace, Name, URL, Type string // Type is "" or "oci"
}

// Inventory is everything read from the cluster in one run.
type Inventory struct {
	Nodes            []Node
	Workloads        []Workload
	HelmReleases     []HelmRelease
	HelmRepositories []HelmRepository
	FluxVersion      string
}

type objMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}

type container struct {
	Image string `json:"image"`
}

type podSpec struct {
	Containers     []container `json:"containers"`
	InitContainers []container `json:"initContainers"`
}

func (p podSpec) images() []string {
	var out []string
	for _, c := range p.Containers {
		out = append(out, c.Image)
	}
	for _, c := range p.InitContainers {
		out = append(out, c.Image)
	}
	return out
}

const helmLabel = "helm.toolkit.fluxcd.io/name"

// Inventory performs every read; any failure is an error.
func (c *Client) Inventory(ctx context.Context) (*Inventory, error) {
	inv := &Inventory{}

	err := c.list(ctx, "/api/v1/nodes", func(raw json.RawMessage) error {
		var n struct {
			Metadata objMeta `json:"metadata"`
			Status   struct {
				NodeInfo struct {
					KubeletVersion string `json:"kubeletVersion"`
					OSImage        string `json:"osImage"`
				} `json:"nodeInfo"`
			} `json:"status"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		inv.Nodes = append(inv.Nodes, Node{Name: n.Metadata.Name, KubeletVersion: n.Status.NodeInfo.KubeletVersion, OSImage: n.Status.NodeInfo.OSImage})
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, w := range []struct{ kind, path string }{
		{"Deployment", "/apis/apps/v1/deployments"},
		{"StatefulSet", "/apis/apps/v1/statefulsets"},
		{"DaemonSet", "/apis/apps/v1/daemonsets"},
	} {
		kind := w.kind
		err := c.list(ctx, w.path, func(raw json.RawMessage) error {
			var o struct {
				Metadata objMeta `json:"metadata"`
				Spec     struct {
					Template struct {
						Spec podSpec `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(raw, &o); err != nil {
				return err
			}
			inv.Workloads = append(inv.Workloads, Workload{
				Kind: kind, Namespace: o.Metadata.Namespace, Name: o.Metadata.Name,
				HelmRelease: o.Metadata.Labels[helmLabel], Images: o.Spec.Template.Spec.images(),
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	err = c.list(ctx, "/apis/batch/v1/cronjobs", func(raw json.RawMessage) error {
		var o struct {
			Metadata objMeta `json:"metadata"`
			Spec     struct {
				JobTemplate struct {
					Spec struct {
						Template struct {
							Spec podSpec `json:"spec"`
						} `json:"template"`
					} `json:"spec"`
				} `json:"jobTemplate"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return err
		}
		inv.Workloads = append(inv.Workloads, Workload{
			Kind: "CronJob", Namespace: o.Metadata.Namespace, Name: o.Metadata.Name,
			HelmRelease: o.Metadata.Labels[helmLabel], Images: o.Spec.JobTemplate.Spec.Template.Spec.images(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = c.list(ctx, "/apis/helm.toolkit.fluxcd.io/v2/helmreleases", func(raw json.RawMessage) error {
		var o struct {
			Metadata objMeta `json:"metadata"`
			Spec     struct {
				Chart struct {
					Spec struct {
						Chart     string `json:"chart"`
						Version   string `json:"version"`
						SourceRef struct {
							Kind      string `json:"kind"`
							Name      string `json:"name"`
							Namespace string `json:"namespace"`
						} `json:"sourceRef"`
					} `json:"spec"`
				} `json:"chart"`
			} `json:"spec"`
			Status struct {
				History []struct {
					ChartVersion string `json:"chartVersion"`
					AppVersion   string `json:"appVersion"`
				} `json:"history"`
			} `json:"status"`
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return err
		}
		cs := o.Spec.Chart.Spec
		hr := HelmRelease{
			Namespace: o.Metadata.Namespace, Name: o.Metadata.Name,
			Chart: cs.Chart, SpecVersion: cs.Version,
			SourceKind: cs.SourceRef.Kind, SourceName: cs.SourceRef.Name, SourceNamespace: cs.SourceRef.Namespace,
		}
		if hr.SourceNamespace == "" {
			hr.SourceNamespace = hr.Namespace
		}
		if len(o.Status.History) > 0 {
			hr.ChartVersion = o.Status.History[0].ChartVersion
			hr.AppVersion = o.Status.History[0].AppVersion
		}
		inv.HelmReleases = append(inv.HelmReleases, hr)
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = c.list(ctx, "/apis/source.toolkit.fluxcd.io/v1/helmrepositories", func(raw json.RawMessage) error {
		var o struct {
			Metadata objMeta `json:"metadata"`
			Spec     struct {
				URL  string `json:"url"`
				Type string `json:"type"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return err
		}
		inv.HelmRepositories = append(inv.HelmRepositories, HelmRepository{
			Namespace: o.Metadata.Namespace, Name: o.Metadata.Name, URL: o.Spec.URL, Type: o.Spec.Type,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	inv.FluxVersion, err = c.fluxVersion(ctx)
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// fluxVersion returns "" (no error) on 404/403 or when no label is found.
func (c *Client) fluxVersion(ctx context.Context) (string, error) {
	version := ""
	err := c.list(ctx, "/apis/apps/v1/namespaces/flux-system/deployments", func(raw json.RawMessage) error {
		if version != "" {
			return nil
		}
		var o struct {
			Metadata objMeta `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return err
		}
		version = o.Metadata.Labels["app.kubernetes.io/version"]
		return nil
	})
	var se *statusError
	if errors.As(err, &se) && (se.status == http.StatusNotFound || se.status == http.StatusForbidden) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return version, nil
}
