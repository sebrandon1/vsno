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
	"io"
	"strconv"
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
	return r.run(ctx, b, Script("registry.sh"), bytes.NewReader(r.Stdin()), args...)
}

// run runs one embedded script through the builder and returns its stdout.
func (r Registry) run(ctx context.Context, b builder.Runner, script string, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	err := b.Run(ctx, script, args, builder.IO{Stdin: stdin, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%w%s", err, builder.Tail(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// SyncOptions is one image copied between the environment's registry and
// another one.
type SyncOptions struct {
	Source      string // where the image is, as a full reference without a scheme
	Destination string // where it goes
	AuthFile    string // the other registry's dockerconfigjson; "" is anonymous
	CAFile      string // the other registry's CA, PEM; "" uses the system trust store
	LocalSource bool   // the environment's registry is Source, so this is a push
	Insecure    bool   // the other registry may be plain HTTP or untrusted
	Overwrite   bool   // replace Destination when it already exists
}

// SyncResult is what the destination holds after a copy.
type SyncResult struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Sync copies one image between the environment's registry and another,
// through the builder: the environment's credentials and CA go on its side,
// the caller's own on the other.
func (r Registry) Sync(ctx context.Context, b builder.Runner, o SyncOptions) (*SyncResult, error) {
	files := map[string]string{"auth.json": r.Auth(), "ca.crt": r.CA}
	if o.AuthFile != "" {
		files["remote-auth.json"] = o.AuthFile
	}
	if o.CAFile != "" {
		files["remote-ca.crt"] = o.CAFile
	}
	t, err := builder.Tar(files)
	if err != nil {
		return nil, fmt.Errorf("packing the registry credentials: %w", err)
	}
	dir := "in"
	if o.LocalSource {
		dir = "out"
	}
	out, err := r.run(ctx, b, Script("sync.sh"), bytes.NewReader(t),
		dir, o.Source, o.Destination, strconv.FormatBool(o.Insecure), strconv.FormatBool(o.Overwrite))
	if err != nil {
		return nil, fmt.Errorf("copying %s: %w", o.Source, err)
	}
	var res SyncResult
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		return nil, fmt.Errorf("reading the copy's result: %w", err)
	}
	return &res, nil
}
