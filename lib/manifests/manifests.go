// Package manifests renders the Kubernetes objects vsno applies to the host
// cluster from embedded templates.
package manifests

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/sebrandon1/vsno/lib/cluster"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Template names, in the order setup applies them.
const (
	Operators       = "operators"
	HyperConverged  = "hyperconverged"
	LVMCluster      = "lvmcluster"
	Namespace       = "namespace"
	DNSMasq         = "dnsmasq"
	Builder         = "builder"
	S3              = "s3"
	RegistryService = "registry-service"
	Registry        = "registry"
)

// Step is one step of setup and the templates it applies, in order. Teardown
// mirrors the same steps in reverse.
type Step struct {
	Name      string
	Templates []string
	// Namespace, if set, keeps only the objects in it and the Namespace itself
	// (the operators template holds both operators).
	Namespace string
	// Host steps install on the host cluster; teardown removes them only with --host.
	Host bool
}

// Step names, shared by setup's report and teardown's mirror of it. StepCNV
// and StepLVMS equal the operators' cluster.Operator keys.
const (
	StepCNV            = "cnv"
	StepLVMS           = "lvms"
	StepHyperConverged = "hyperconverged"
	StepStorage        = "storage"
	StepNamespace      = "namespace"
	StepSupport        = "support"
)

// Steps are setup's steps, in order.
var Steps = []Step{
	{Name: StepCNV, Templates: []string{Operators}, Namespace: "openshift-cnv", Host: true},
	{Name: StepLVMS, Templates: []string{Operators}, Namespace: "openshift-storage", Host: true},
	{Name: StepHyperConverged, Templates: []string{HyperConverged}, Host: true},
	{Name: StepStorage, Templates: []string{LVMCluster}, Host: true},
	{Name: StepNamespace, Templates: []string{Namespace}},
	{Name: StepSupport, Templates: []string{DNSMasq, Builder, RegistryService, S3, Registry}},
}

// Objects renders the step's templates and keeps the step's objects.
func (s Step) Objects(cfg Config) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, t := range s.Templates {
		objs, err := Render(t, cfg)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			if s.Namespace == "" || o.GetNamespace() == s.Namespace || (o.GetKind() == "Namespace" && o.GetName() == s.Namespace) {
				out = append(out, o)
			}
		}
	}
	return out, nil
}

// NamesConfig is enough Config to render every object's kind, namespace and
// name for ns, e.g. to find what setup created there.
func NamesConfig(ns string) Config {
	return Config{Namespace: ns, BaseDomain: DefaultBaseDomain,
		StorageClass: DefaultStorageClass, Images: DefaultImages(), S3Bucket: DefaultS3Bucket}
}

// Defaults shared with the commands that consume these manifests.
const (
	DefaultBaseDomain   = "vsno.lab"
	DefaultStorageClass = "lvms-vg-vsno"
	DefaultS3Bucket     = "ibu"
	// GuestIP is the address every KubeVirt masquerade guest has for itself.
	GuestIP = "10.0.2.2"
)

//go:embed templates/*.yaml.tmpl
var templates embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	// quote emits a double-quoted YAML scalar; JSON string escaping is valid YAML.
	"quote": strconv.Quote,
}).ParseFS(templates, "templates/*.yaml.tmpl"))

// Images are the container images the support pods run.
type Images struct {
	Base      string // dnsmasq and builder (CentOS Stream with dnf)
	SeaweedFS string
	Registry  string
}

// DefaultImages pins the images verified with the manual procedure.
func DefaultImages() Images {
	return Images{
		Base:      "quay.io/centos/centos:stream9",
		SeaweedFS: "docker.io/chrislusf/seaweedfs:4.54",
		Registry:  "quay.io/libpod/registry:2.8.2",
	}
}

// RegistryAuth holds the in-cluster registry's TLS material (PEM) and credentials.
type RegistryAuth struct {
	CACert, CAKey string
	Cert, Key     string
	User          string
	Password      string
	Htpasswd      string
}

// Config is the input to every template.
type Config struct {
	Namespace    string
	Node         string
	DiskByID     string
	HostDNS      string
	CNVChannel   string
	LVMSChannel  string
	BaseDomain   string
	StorageClass string
	Images       Images

	// SSHPrivateKey and SSHPublicKey are the builder's key for guest SSH.
	SSHPrivateKey, SSHPublicKey string

	S3AccessKey string
	S3SecretKey string
	S3Bucket    string

	Registry RegistryAuth
}

// GuestIP is exposed to templates.
func (Config) GuestIP() string { return GuestIP }

// GuestsConfigMap holds one dnsmasq file per vSNO; templates use it.
func (Config) GuestsConfigMap() string { return GuestsConfigMap }

// GuestsConfigMap is the ConfigMap with one dnsmasq file per vSNO.
const GuestsConfigMap = "vsno-guests"

// GuestDNS is the dnsmasq file for vSNO name: api, api-int and *.apps resolve to
// GuestIP, the address each masquerade guest has for itself. A wildcard on the
// whole cluster domain would fail the agent installer's
// dns-wildcard-not-configured validation.
func GuestDNS(name, baseDomain string) string {
	var b strings.Builder
	for _, host := range []string{"api", "api-int", "apps"} {
		fmt.Fprintf(&b, "address=/%s.%s.%s/%s\n", host, name, baseDomain, GuestIP)
	}
	return b.String()
}

// S3ConfigJSON is the SeaweedFS identity file for the generated access key.
func (c Config) S3ConfigJSON() (string, error) {
	type cred struct {
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
	}
	type identity struct {
		Name        string   `json:"name"`
		Credentials []cred   `json:"credentials"`
		Actions     []string `json:"actions"`
	}
	b, err := json.Marshal(map[string][]identity{"identities": {{
		Name:        "vsno",
		Credentials: []cred{{c.S3AccessKey, c.S3SecretKey}},
		Actions:     []string{"Admin", "Read", "Write", "List", "Tagging"},
	}}})
	return string(b), err
}

// RenderYAML renders one template to a multi-document YAML string.
func RenderYAML(name string, cfg Config) (string, error) {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name+".yaml.tmpl", cfg); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, err)
	}
	return b.String(), nil
}

// Render renders one template and parses it into objects (see Parse).
func Render(name string, cfg Config) ([]*unstructured.Unstructured, error) {
	s, err := RenderYAML(name, cfg)
	if err != nil {
		return nil, err
	}
	return Parse(name, s)
}

// Parse splits multi-document YAML into objects, each labeled
// app.kubernetes.io/managed-by=vsno. Owning that label keeps vsno's field
// manager on every object it applies, even one whose only other fields are
// set by controllers (a ServiceAccount, for example), so a later dry run can
// tell vsno's objects from someone else's. what names the source in errors.
func Parse(what, s string) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured
	for i, doc := range strings.Split(s, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		j, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			return nil, fmt.Errorf("parsing %s document %d: %w", what, i, err)
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(j); err != nil {
			return nil, fmt.Errorf("parsing %s document %d: %w", what, i, err)
		}
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[cluster.ManagedByLabel] = "vsno"
		u.SetLabels(labels)
		objs = append(objs, u)
	}
	return objs, nil
}
