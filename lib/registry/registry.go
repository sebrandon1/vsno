// Package registry is the environment's in-cluster OCI registry: where vsno
// keeps seed images and mirrored OpenShift releases. The builder reaches it by
// Service name; guests reach it by ClusterIP, because their own cluster.local
// hides the host's Service names (the certificate covers the IP).
package registry

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Service is the registry's Service and Deployment name.
const Service = "registry"

// Registry is one environment's registry.
type Registry struct {
	Host     string // registry.<ns>.svc.cluster.local:5000, as guests and the builder reach it
	User     string
	Password string
	CA       string // PEM
}

// Host is the registry's address in namespace ns.
func Host(ns string) string {
	return fmt.Sprintf("registry.%s.svc.cluster.local:%d", ns, cluster.RegistryPort)
}

// Load reads the registry's credentials and CA, which vsno setup stored.
func Load(ctx context.Context, kube kubernetes.Interface, ns string) (Registry, error) {
	r := Registry{Host: Host(ns)}
	read := func(name string) (map[string][]byte, error) {
		s, err := kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %s/%s is missing; rerun `vsno setup -n %s`", ns, name, ns)
		}
		if err != nil {
			return nil, fmt.Errorf("reading secret %s: %w", name, err)
		}
		return s.Data, nil
	}
	auth, err := read("registry-auth")
	if err != nil {
		return r, err
	}
	ca, err := read("registry-ca")
	if err != nil {
		return r, err
	}
	r.User, r.Password, r.CA = string(auth["username"]), string(auth["password"]), string(ca["ca.crt"])
	if r.User == "" || r.Password == "" || r.CA == "" {
		return r, fmt.Errorf("the registry credentials in %s are incomplete; rerun `vsno setup -n %s`", ns, ns)
	}
	return r, nil
}

// Address is the registry as a guest reaches it: <clusterIP>:5000.
func Address(ctx context.Context, kube kubernetes.Interface, ns string) (string, error) {
	svc, err := kube.CoreV1().Services(ns).Get(ctx, Service, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("service %s/%s is missing; rerun `vsno setup -n %s`", ns, Service, ns)
	}
	if err != nil {
		return "", fmt.Errorf("reading Service %s: %w", Service, err)
	}
	return fmt.Sprintf("%s:%d", svc.Spec.ClusterIP, cluster.RegistryPort), nil
}

// Auth is a dockerconfigjson with the registry's credentials.
func (r Registry) Auth() string {
	return DockerConfig(r.Host, base64.StdEncoding.EncodeToString([]byte(r.User+":"+r.Password)))
}

// DockerConfig is a dockerconfigjson with one host's credentials.
func DockerConfig(host, auth string) string {
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": auth}}})
	return string(b)
}

// AddAuth returns dockerConfig with host's credentials added, keeping every
// other entry: a guest's pull secret needs the registry alongside the upstream
// registries its release comes from.
func AddAuth(dockerConfig, host, user, password string) (string, error) {
	var cfg map[string]any
	if err := json.Unmarshal([]byte(dockerConfig), &cfg); err != nil {
		return "", fmt.Errorf("parsing the pull secret: %w", err)
	}
	auths, _ := cfg["auths"].(map[string]any)
	if auths == nil {
		auths = map[string]any{}
		cfg["auths"] = auths
	}
	auths[host] = map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + password))}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("building the pull secret: %w", err)
	}
	return string(b), nil
}

// TrustKey is the registry's key in a guest's additionalTrustedCA ConfigMap:
// host..port.
func (r Registry) TrustKey() string {
	return strings.Replace(r.Host, ":", "..", 1)
}

// Stdin is registry.sh's input.
func (r Registry) Stdin() []byte {
	b, _ := json.Marshal(map[string]string{"user": r.User, "password": r.Password, "ca": r.CA})
	return b
}

//go:embed scripts/*.sh
var files embed.FS

// Script returns an embedded builder script.
func Script(name string) string {
	b, err := files.ReadFile("scripts/" + name)
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(b)
}

// Run runs registry.sh through the builder with the registry's credentials on
// stdin, and returns its stdout.
func (r Registry) Run(ctx context.Context, b builder.Runner, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	err := b.Run(ctx, Script("registry.sh"), args, builder.IO{Stdin: bytes.NewReader(r.Stdin()), Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%w%s", err, builder.Tail(stderr.String()))
	}
	return stdout.Bytes(), nil
}
