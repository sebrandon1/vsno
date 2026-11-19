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
	Guests       []string
	StorageClass string
	Images       Images

	S3AccessKey string
	S3SecretKey string
	S3Bucket    string

	Registry RegistryAuth
}

// GuestIP is exposed to templates.
func (Config) GuestIP() string { return GuestIP }

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

// Render renders one template and parses it into objects.
func Render(name string, cfg Config) ([]*unstructured.Unstructured, error) {
	s, err := RenderYAML(name, cfg)
	if err != nil {
		return nil, err
	}
	var objs []*unstructured.Unstructured
	for i, doc := range strings.Split(s, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		j, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			return nil, fmt.Errorf("parsing %s document %d: %w", name, i, err)
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(j); err != nil {
			return nil, fmt.Errorf("parsing %s document %d: %w", name, i, err)
		}
		objs = append(objs, u)
	}
	return objs, nil
}
