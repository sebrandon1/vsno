// Package kube builds Kubernetes clients for the host cluster.
package kube

import (
	"fmt"
	"net/http"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sebrandon1/vsno/lib/trace"
)

// Clients bundles the clients vsno uses against the host cluster.
type Clients struct {
	Config *rest.Config
	Kube   kubernetes.Interface
	Dyn    dynamic.Interface
	// Context is the kubeconfig context in use, for display.
	Context string
}

// New loads a kubeconfig the same way oc/kubectl do: explicit path, then
// $KUBECONFIG, then ~/.kube/config.
func New(kubeconfig, context string) (*Clients, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: context}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig: %w", err)
	}
	ctxName := raw.CurrentContext
	if context != "" {
		ctxName = context
	}
	wrapTransport(cfg)

	k, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}
	d, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}
	return &Clients{Config: cfg, Kube: k, Dyn: d, Context: ctxName}, nil
}

// wrapTransport installs the verbose trace on every client built from cfg: the
// core, dynamic and discovery clients and the pod executors all share it. It is
// installed unconditionally and checks whether tracing is on at request time, so
// the flag may be parsed before or after the client is built.
func wrapTransport(cfg *rest.Config) {
	inner := cfg.WrapTransport
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if inner != nil {
			rt = inner(rt)
		}
		return trace.RoundTripper(rt)
	}
}
