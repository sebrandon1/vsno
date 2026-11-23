package kube

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// A config that already wraps its transport keeps that wrapper, and the trace
// goes outermost.
func TestWrapTransport(t *testing.T) {
	wrapped := false
	cfg := &rest.Config{Host: "https://example.test"}
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		wrapped = true
		return rt
	}
	wrapTransport(cfg)
	if rt := cfg.WrapTransport(http.DefaultTransport); rt == nil {
		t.Fatal("WrapTransport returned nil")
	}
	if !wrapped {
		t.Error("the existing wrapper was dropped")
	}
}

func TestWrapTransportWithoutOne(t *testing.T) {
	cfg := &rest.Config{Host: "https://example.test"}
	wrapTransport(cfg)
	if cfg.WrapTransport == nil {
		t.Fatal("WrapTransport was not installed")
	}
	if rt := cfg.WrapTransport(http.DefaultTransport); rt == nil {
		t.Fatal("WrapTransport returned nil")
	}
}

// kubeconfig writes a two-context kubeconfig to a temp file.
func kubeconfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubeconfig")
	const cfg = `apiVersion: v1
kind: Config
clusters:
- name: one
  cluster:
    server: https://api.one.test:6443
- name: two
  cluster:
    server: https://api.two.test:6443
contexts:
- name: one
  context:
    cluster: one
    user: u
- name: two
  context:
    cluster: two
    user: u
current-context: one
users:
- name: u
  user: {}
`
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// New is the only place vsno builds a client: the whole CLI's access to the
// host cluster, and where the verbose trace is attached.
func TestNew(t *testing.T) {
	path := kubeconfig(t)
	c, err := New(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kube == nil || c.Dyn == nil || c.Config == nil {
		t.Fatalf("clients = %+v, want them built", c)
	}
	if c.Context != "one" || c.Config.Host != "https://api.one.test:6443" {
		t.Errorf("context = %q, host = %q", c.Context, c.Config.Host)
	}
	if c.Config.WrapTransport == nil {
		t.Error("the trace transport was not installed")
	}

	// A --context override picks another context from the same file.
	c, err = New(path, "two")
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != "two" || c.Config.Host != "https://api.two.test:6443" {
		t.Errorf("context = %q, host = %q", c.Context, c.Config.Host)
	}
}

func TestNewMissingKubeconfig(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "absent"), "")
	if err == nil || !strings.Contains(err.Error(), "loading kubeconfig") {
		t.Errorf("got %v", err)
	}
}
