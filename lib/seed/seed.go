// Package seed turns vSNOs into Image Based Upgrade seed images and manages
// those images in the environment's registry. Seed generation itself is done
// by the Lifecycle Agent (LCA) on the vSNO: it stops the cluster and builds the
// image on the node, so vsno installs LCA, configures it and follows it.
package seed

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/registry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// RepoPrefix is where seed images live in the environment's registry:
	// seeds/<vSNO name>:<OpenShift version>.
	RepoPrefix = "seeds/"
	// AnnSeedImage on a vSNO's VM is the last seed image made from it.
	AnnSeedImage = "vsno.io/seed-image"
	// LCANamespace is where the Lifecycle Agent runs on the vSNO.
	LCANamespace = "openshift-lifecycle-agent"
	// LabelClusterInfo and LabelFormat are LCA's labels on a seed image.
	LabelClusterInfo = "com.openshift.lifecycle-agent.seed_cluster_info"
	LabelFormat      = "com.openshift.lifecycle-agent.seed_format_version"

	trustConfigMap = "vsno-registry-ca"
	registryPort   = 5000
)

// Env is what the seed commands need: the guest package's cluster access, and
// oc / ssh to a vSNO.
type Env struct {
	guest.Env
	// Access returns oc and ssh for vSNO name; nil means through the builder.
	Access func(name string) Access
	// Volume reads the environment registry's used and total bytes. Nil means
	// the caller can't tell, and the precheck then says nothing about space
	// rather than inventing a number.
	Volume func(ctx context.Context, ns string) (used, total int64, err error)
}

// Defaults fills in what e leaves unset: silent progress, a 10s poll, an
// hour's timeout, the real clock, and access through the builder of ns.
func (e *Env) Defaults(ns string) {
	if e.Progress == nil {
		e.Progress = precheck.NoProgress()
	}
	if e.Poll == 0 {
		e.Poll = 10 * time.Second
	}
	if e.Timeout == 0 {
		e.Timeout = time.Hour
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Access == nil {
		ge := e.Env
		e.Access = func(name string) Access { return guestctl.ThroughBuilder(ge, ns, name) }
	}
}

// Access runs oc and ssh on one vSNO.
type Access = guestctl.Access

// Registry is the environment's in-cluster registry.
type Registry = registry.Registry

// RegistryHost is the registry's address in namespace ns.
func RegistryHost(ns string) string { return registry.Host(ns) }

// LoadRegistry reads the registry's credentials and CA, which vsno setup stored.
func LoadRegistry(ctx context.Context, kube kubernetes.Interface, ns string) (Registry, error) {
	return registry.Load(ctx, kube, ns)
}

// Ref is the default seed image of vSNO name at version.
func Ref(r Registry, name, version string) string {
	return fmt.Sprintf("%s/%s%s:%s", r.Host, RepoPrefix, name, version)
}

// imageHost is the registry part of an image reference.
func imageHost(ref string) string {
	host, _, _ := strings.Cut(ref, "/")
	return host
}

// seedAuth is the push credential for image: the environment registry's, or the
// host cluster pull secret's entry for an external registry.
func seedAuth(ctx context.Context, kube kubernetes.Interface, r Registry, image string) (string, error) {
	host := imageHost(image)
	if host == r.Host {
		return r.Auth(), nil
	}
	ps, err := kube.CoreV1().Secrets("openshift-config").Get(ctx, "pull-secret", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the cluster pull secret: %w", err)
	}
	var cfg struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(ps.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
		return "", fmt.Errorf("parsing the cluster pull secret: %w", err)
	}
	entry, ok := cfg.Auths[host]
	if !ok {
		return "", fmt.Errorf("the cluster pull secret (openshift-config/pull-secret) has no credentials for %s, so the seed can't be pushed there", host)
	}
	b, _ := json.Marshal(map[string]any{"auths": map[string]json.RawMessage{host: entry}})
	return string(b), nil
}

//go:embed templates/*.tmpl
var files embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{"quote": strconv.Quote}).
	ParseFS(files, "templates/*.tmpl"))

// renderData is the input to every template.
type renderData struct {
	LCANamespace   string
	TrustConfigMap string
	TrustKey       string
	CA             string
	SeedAuth       string
	Image          string
}

func render(name string, d renderData) (string, error) {
	d.LCANamespace, d.TrustConfigMap = LCANamespace, trustConfigMap
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, d); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, err)
	}
	return b.String(), nil
}
