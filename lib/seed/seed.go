// Package seed turns vSNOs into Image Based Upgrade seed images and manages
// those images in the environment's registry. Seed generation itself is done
// by the Lifecycle Agent (LCA) on the vSNO: it stops the cluster and builds the
// image on the node, so vsno installs LCA, configures it and follows it.
package seed

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/precheck"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
}

func (e *Env) defaults(ns string) {
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
		e.Access = func(name string) Access { return builderAccess{e: ge, ns: ns, name: name} }
	}
}

// Access runs oc and ssh on one vSNO.
type Access interface {
	Oc(ctx context.Context, args []string, s builder.IO) error
	SSH(ctx context.Context, args []string, s builder.IO) error
}

// builderAccess reaches the vSNO through the builder pod, like vsno oc and vsno ssh.
type builderAccess struct {
	e        guest.Env
	ns, name string
}

func (b builderAccess) Oc(ctx context.Context, args []string, s builder.IO) error {
	return guest.Oc(ctx, b.e, b.ns, b.name, args, s)
}

func (b builderAccess) SSH(ctx context.Context, args []string, s builder.IO) error {
	return guest.SSH(ctx, b.e, b.ns, b.name, args, s)
}

// Registry is the environment's in-cluster registry.
type Registry struct {
	Host     string // registry.<ns>.svc.cluster.local:5000, as guests and the builder reach it
	User     string
	Password string
	CA       string // PEM
}

// RegistryHost is the registry's address in namespace ns.
func RegistryHost(ns string) string {
	return fmt.Sprintf("registry.%s.svc.cluster.local:%d", ns, registryPort)
}

// LoadRegistry reads the registry's credentials and CA, which vsno setup stored.
func LoadRegistry(ctx context.Context, kube kubernetes.Interface, ns string) (Registry, error) {
	r := Registry{Host: RegistryHost(ns)}
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

// Auth is a dockerconfigjson with the registry's credentials.
func (r Registry) Auth() string {
	return dockerConfig(r.Host, base64.StdEncoding.EncodeToString([]byte(r.User+":"+r.Password)))
}

func dockerConfig(host, auth string) string {
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": auth}}})
	return string(b)
}

// stdin is the registry script's input.
func (r Registry) stdin() []byte {
	b, _ := json.Marshal(map[string]string{"user": r.User, "password": r.Password, "ca": r.CA})
	return b
}

// TrustKey is the registry's key in the guest's additionalTrustedCA ConfigMap:
// host..port.
func (r Registry) TrustKey() string {
	return strings.Replace(r.Host, ":", "..", 1)
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

//go:embed templates/*.tmpl scripts/*.sh
var files embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{"quote": strconv.Quote}).
	ParseFS(files, "templates/*.tmpl"))

// renderData is the input to every template.
type renderData struct {
	LCANamespace    string
	Channel         string
	Source          string
	SourceNamespace string
	TrustConfigMap  string
	TrustKey        string
	CA              string
	SeedAuth        string
	Image           string
}

func render(name string, d renderData) (string, error) {
	d.LCANamespace, d.TrustConfigMap = LCANamespace, trustConfigMap
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, d); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, err)
	}
	return b.String(), nil
}

func script(name string) string {
	b, err := files.ReadFile("scripts/" + name)
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(b)
}

// ocJSON runs oc get ... -o json on the vSNO into out. With --ignore-not-found
// in args, a missing object gives found == false.
func ocJSON(ctx context.Context, a Access, out any, args ...string) (bool, error) {
	var stdout, stderr bytes.Buffer
	if err := a.Oc(ctx, append(args, "-o", "json"), builder.IO{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return false, ocErr(args, err, stderr.String())
	}
	if len(bytes.TrimSpace(stdout.Bytes())) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return false, fmt.Errorf("oc %s: parsing the output: %w", strings.Join(args, " "), err)
	}
	return true, nil
}

// oc runs oc on the vSNO with stdin, discarding its output.
func oc(ctx context.Context, a Access, stdin string, args ...string) error {
	var stderr bytes.Buffer
	s := builder.IO{Stdout: &bytes.Buffer{}, Stderr: &stderr}
	if stdin != "" {
		s.Stdin = strings.NewReader(stdin)
	}
	if err := a.Oc(ctx, args, s); err != nil {
		return ocErr(args, err, stderr.String())
	}
	return nil
}

func ocErr(args []string, err error, stderr string) error {
	msg := fmt.Sprintf("oc %s: %v", strings.Join(args, " "), err)
	if s := strings.TrimSpace(stderr); s != "" {
		msg += ": " + s[strings.LastIndex(s, "\n")+1:]
	}
	return fmt.Errorf("%s", msg)
}

// condition is a status condition as oc prints it.
type condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// object is the part of any listed object vsno reads.
type object struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Phase      string      `json:"phase"`
		Conditions []condition `json:"conditions"`
	} `json:"status"`
}

type objectList struct {
	Items []object `json:"items"`
}

func (o object) cond(t string) *condition {
	for i := range o.Status.Conditions {
		if o.Status.Conditions[i].Type == t {
			return &o.Status.Conditions[i]
		}
	}
	return nil
}

func (o object) isTrue(t string) bool {
	c := o.cond(t)
	return c != nil && c.Status == "True"
}
