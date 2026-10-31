// Package cluster reads the versions of things running in a Kubernetes
// cluster through the plain REST API. It is read-only (GET only) and decodes
// only the fields version-monitor needs.
package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// saDir is the in-cluster ServiceAccount directory; tests override it.
var saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

const pageLimit = "500"

// Client is a minimal read-only Kubernetes API client.
type Client struct {
	base  string
	http  *http.Client
	token string
}

// New builds a client with an explicit base URL and HTTP client (for tests).
// token is optional.
func New(baseURL string, httpClient *http.Client, token string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), http: httpClient, token: token}
}

func newHTTP(tlsCfg *tls.Config) *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSClientConfig:     tlsCfg,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// NewInCluster builds a client from the ServiceAccount mounted into the pod.
func NewInCluster() (*Client, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("cluster: not running in a cluster: KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT not set")
	}
	tok, err := os.ReadFile(filepath.Join(saDir, "token"))
	if err != nil {
		return nil, fmt.Errorf("cluster: reading ServiceAccount token: %w", err)
	}
	caPath := filepath.Join(saDir, "ca.crt")
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("cluster: reading ServiceAccount CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("cluster: no valid certificates in %s", caPath)
	}
	base := "https://" + net.JoinHostPort(host, port)
	return New(base, newHTTP(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}), strings.TrimSpace(string(tok))), nil
}

type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                string `yaml:"server"`
			CAData                string `yaml:"certificate-authority-data"`
			CAFile                string `yaml:"certificate-authority"`
			InsecureSkipTLSVerify bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token        string         `yaml:"token"`
			TokenFile    string         `yaml:"token-file"`
			CertData     string         `yaml:"client-certificate-data"`
			KeyData      string         `yaml:"client-key-data"`
			CertFile     string         `yaml:"client-certificate"`
			KeyFile      string         `yaml:"client-key"`
			Username     string         `yaml:"username"`
			Password     string         `yaml:"password"`
			Exec         map[string]any `yaml:"exec"`
			AuthProvider map[string]any `yaml:"auth-provider"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// NewFromKubeconfig builds a client from a kubeconfig file. contextName ""
// selects current-context.
func NewFromKubeconfig(path, contextName string) (*Client, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cluster: reading kubeconfig: %w", err)
	}
	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("cluster: parsing kubeconfig %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	rel := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}

	if contextName == "" {
		contextName = kc.CurrentContext
	}
	if contextName == "" {
		return nil, errors.New("cluster: kubeconfig has no current-context and none was given")
	}
	var clusterName, userName string
	found := false
	for _, c := range kc.Contexts {
		if c.Name == contextName {
			clusterName, userName, found = c.Context.Cluster, c.Context.User, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("cluster: context %q not found in kubeconfig", contextName)
	}

	ci := -1
	for i, c := range kc.Clusters {
		if c.Name == clusterName {
			ci = i
			break
		}
	}
	if ci < 0 {
		return nil, fmt.Errorf("cluster: cluster %q (context %q) not found in kubeconfig", clusterName, contextName)
	}
	cl := kc.Clusters[ci].Cluster
	if cl.Server == "" {
		return nil, fmt.Errorf("cluster: cluster %q has no server", clusterName)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cl.InsecureSkipTLSVerify}
	var caPEM []byte
	switch {
	case cl.CAData != "":
		caPEM, err = base64.StdEncoding.DecodeString(cl.CAData)
		if err != nil {
			return nil, fmt.Errorf("cluster: certificate-authority-data: %w", err)
		}
	case cl.CAFile != "":
		caPEM, err = os.ReadFile(rel(cl.CAFile))
		if err != nil {
			return nil, fmt.Errorf("cluster: reading certificate-authority: %w", err)
		}
	}
	if caPEM != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("cluster: no valid certificates in kubeconfig certificate authority")
		}
		tlsCfg.RootCAs = pool
	}

	var token string
	if userName != "" {
		ui := -1
		for i, u := range kc.Users {
			if u.Name == userName {
				ui = i
				break
			}
		}
		if ui < 0 {
			return nil, fmt.Errorf("cluster: user %q (context %q) not found in kubeconfig", userName, contextName)
		}
		u := kc.Users[ui].User
		if u.Exec != nil {
			return nil, fmt.Errorf("cluster: kubeconfig user %q uses exec credentials, which are not supported", userName)
		}
		if u.AuthProvider != nil {
			return nil, fmt.Errorf("cluster: kubeconfig user %q uses an auth-provider, which is not supported", userName)
		}
		if u.Username != "" || u.Password != "" {
			return nil, fmt.Errorf("cluster: kubeconfig user %q uses username/password auth, which is not supported", userName)
		}
		token = u.Token
		if token == "" && u.TokenFile != "" {
			b, err := os.ReadFile(rel(u.TokenFile))
			if err != nil {
				return nil, fmt.Errorf("cluster: reading token-file: %w", err)
			}
			token = strings.TrimSpace(string(b))
		}
		var certPEM, keyPEM []byte
		if u.CertData != "" || u.KeyData != "" {
			if certPEM, err = base64.StdEncoding.DecodeString(u.CertData); err != nil {
				return nil, fmt.Errorf("cluster: client-certificate-data: %w", err)
			}
			if keyPEM, err = base64.StdEncoding.DecodeString(u.KeyData); err != nil {
				return nil, fmt.Errorf("cluster: client-key-data: %w", err)
			}
		} else if u.CertFile != "" || u.KeyFile != "" {
			if certPEM, err = os.ReadFile(rel(u.CertFile)); err != nil {
				return nil, fmt.Errorf("cluster: reading client-certificate: %w", err)
			}
			if keyPEM, err = os.ReadFile(rel(u.KeyFile)); err != nil {
				return nil, fmt.Errorf("cluster: reading client-key: %w", err)
			}
		}
		if certPEM != nil {
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return nil, fmt.Errorf("cluster: loading client certificate: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{pair}
		}
	}
	return New(cl.Server, newHTTP(tlsCfg), token), nil
}

type statusError struct {
	method string
	path   string
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.method, e.path, e.status, e.body)
}

// get performs a GET and returns the body for 2xx responses.
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "version-monitor")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading body: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := body
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, &statusError{method: http.MethodGet, path: path, status: resp.StatusCode, body: string(snippet)}
	}
	return body, nil
}

// list fetches every page of a list endpoint, calling each with the raw items.
func (c *Client) list(ctx context.Context, path string, each func(raw json.RawMessage) error) error {
	cont := ""
	for {
		q := url.Values{"limit": {pageLimit}}
		if cont != "" {
			q.Set("continue", cont)
		}
		body, err := c.get(ctx, path, q)
		if err != nil {
			return err
		}
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("GET %s: decoding response: %w", path, err)
		}
		for _, it := range page.Items {
			if err := each(it); err != nil {
				return fmt.Errorf("GET %s: decoding item: %w", path, err)
			}
		}
		if page.Metadata.Continue == "" {
			return nil
		}
		cont = page.Metadata.Continue
	}
}
