// Package guest creates, lists, deletes and reaches vSNOs: single-node
// OpenShift clusters installed with the agent-based installer into KubeVirt
// VMs in the environment namespace. The VMs themselves are the inventory.
package guest

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"text/template"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/release"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Labels and annotations on a vSNO's objects.
const (
	// LabelName marks every object of a vSNO with its name; the VM with it is the vSNO.
	LabelName = "vsno.io/name"
	// AnnRequested is the --version the user asked for.
	AnnRequested = "vsno.io/version-requested"
	// AnnImage is the release image it was installed from.
	AnnImage = "vsno.io/release-image"
	// AnnVersion is the OpenShift version: known up front for a version, after
	// install for a pullspec.
	AnnVersion = "vsno.io/version"
	// AnnCached is "true" when the guest installed from the registry mirror.
	AnnCached = "vsno.io/cache"
	// AnnTimingISO, AnnTimingInstall and AnnTimingReboot are how long each phase
	// of a create took, and AnnTimingAvailable the whole create up to the guest's
	// ClusterVersion being Available, as Go durations ("8m12s").
	AnnTimingISO       = "vsno.io/timing-iso"
	AnnTimingInstall   = "vsno.io/timing-install"
	AnnTimingReboot    = "vsno.io/timing-reboot"
	AnnTimingAvailable = "vsno.io/timing-available"
)

// Sizes. The default memory is single-node OpenShift's minimum, so more vSNOs
// fit; heavier work such as IBU's precache and seed generation wants 24-32 GiB
// (--memory). vCPUs cost little: KubeVirt requests a tenth of each by default.
const (
	DefaultCores     = 16
	DefaultMemoryGiB = 16
	DefaultDiskGiB   = 200
	MinCores         = 8
	MinMemoryGiB     = 16
	MinDiskGiB       = 120
	// gateway is the masquerade network's gateway, as the guest sees it.
	gateway = "10.0.2.1"
	// containersStartMiB is where /var/lib/containers starts on a default-size disk.
	containersStartMiB = 100 * 1024
)

// Spec is a vSNO to create.
type Spec struct {
	Namespace string
	Name      string
	Node      string
	Release   release.Release
	Cores     int
	MemoryGiB int64
	DiskGiB   int64
}

// Names a vSNO can't take: the support services' Services and volumes use them.
var reserved = []string{"dnsmasq", "builder", "s3", "registry"}

var reName = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// ValidateName checks that name is a usable vSNO name: a DNS label of at most
// 30 characters (it becomes the cluster name and api.<name>.<base domain>)
// that doesn't clash with the support services.
func ValidateName(name string) error {
	switch {
	case len(name) > 30 || !reName.MatchString(name):
		return fmt.Errorf("invalid name %q: use up to 30 lowercase letters, digits and '-', starting with a letter", name)
	case slices.Contains(reserved, name):
		return fmt.Errorf("name %q is reserved for a support service; pick another", name)
	}
	return nil
}

// Validate checks the name and size.
func (s Spec) Validate() error {
	if err := ValidateName(s.Name); err != nil {
		return err
	}
	switch {
	case s.Cores < MinCores:
		return fmt.Errorf("--cores %d is below the single-node OpenShift minimum of %d", s.Cores, MinCores)
	case s.MemoryGiB < MinMemoryGiB:
		return fmt.Errorf("--memory %d GiB is below the single-node OpenShift minimum of %d GiB", s.MemoryGiB, MinMemoryGiB)
	case s.DiskGiB < MinDiskGiB:
		return fmt.Errorf("--disk %d GiB is below the single-node OpenShift minimum of %d GiB", s.DiskGiB, MinDiskGiB)
	}
	return nil
}

// MAC is a stable, locally administered MAC address for the vSNO.
func MAC(ns, name string) string {
	h := sha256.Sum256([]byte(ns + "/" + name))
	return fmt.Sprintf("02:76:73:%02x:%02x:%02x", h[0], h[1], h[2])
}

// containersStart is where /var/lib/containers begins: 100 GiB in, or halfway
// into a smaller disk.
func containersStart(diskGiB int64) int64 {
	return min(containersStartMiB, diskGiB*1024/2)
}

// ISOURL is where the VM imports the vSNO's agent ISO from: the builder's web server.
func ISOURL(ns, name string) string {
	return fmt.Sprintf("http://builder.%s.svc:8080/%s.iso", ns, name)
}

//go:embed templates/*.tmpl scripts/*.sh
var files embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{"quote": strconv.Quote}).
	ParseFS(files, "templates/*.tmpl"))

// renderData is the input to every template.
type renderData struct {
	Spec
	LabelName    string
	BaseDomain   string
	GuestIP      string
	Gateway      string
	DNS          string // the dnsmasq Service's ClusterIP
	MAC          string
	PullSecret   string
	SSHPublicKey string
	MachineCIDR  string
	ClusterCIDR  string
	ServiceCIDR  string
	ISOURL       string
	StorageClass string
	Annotations  map[string]string

	ContainersStartMiB int64

	// The environment's registry as a mirror for the install; empty means the
	// guest pulls the release from the internet.
	ImageDigestSources    []cache.ImageDigestSource
	AdditionalTrustBundle string
}

// Secrets are what the install config needs from the cluster.
type Secrets struct {
	PullSecret   string // the host's global pull secret, compact JSON
	SSHPublicKey string // the builder's key (vsno-ssh)
	// ImageDigestSources and AdditionalTrustBundle point the install at the
	// environment's registry; empty means the guest pulls from the internet.
	ImageDigestSources    []cache.ImageDigestSource
	AdditionalTrustBundle string
}

func newRenderData(s Spec, dnsIP string, sec Secrets) renderData {
	req := precheck.DefaultRequirements()
	ann := map[string]string{AnnRequested: s.Release.Requested, AnnImage: s.Release.Image}
	if s.Release.Version != "" {
		ann[AnnVersion] = s.Release.Version
	}
	if len(sec.ImageDigestSources) > 0 {
		// Known before the install finishes, so a cache user is visible (and
		// protected from `cache prune`) from the moment its VM exists.
		ann[AnnCached] = "true"
	}
	return renderData{
		Spec: s, LabelName: LabelName, BaseDomain: manifests.DefaultBaseDomain,
		GuestIP: manifests.GuestIP, Gateway: gateway, DNS: dnsIP, MAC: MAC(s.Namespace, s.Name),
		PullSecret: sec.PullSecret, SSHPublicKey: sec.SSHPublicKey,
		MachineCIDR: req.GuestMachineCIDR, ClusterCIDR: req.GuestClusterCIDR, ServiceCIDR: req.GuestServiceCIDR,
		ISOURL: ISOURL(s.Namespace, s.Name), StorageClass: manifests.DefaultStorageClass, Annotations: ann,
		ContainersStartMiB: containersStart(s.DiskGiB),
		ImageDigestSources: sec.ImageDigestSources, AdditionalTrustBundle: sec.AdditionalTrustBundle,
	}
}

func render(name string, d renderData) (string, error) {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, d); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, err)
	}
	return b.String(), nil
}

// Objects are the vSNO's Service, ISO DataVolume and VM.
func Objects(s Spec, dnsIP string, sec Secrets) ([]*unstructured.Unstructured, error) {
	y, err := render("vm.yaml.tmpl", newRenderData(s, dnsIP, sec))
	if err != nil {
		return nil, err
	}
	return manifests.Parse("vSNO "+s.Name, y)
}

// InstallFiles are the agent installer's inputs, by path in its directory.
func InstallFiles(s Spec, dnsIP string, sec Secrets) (map[string]string, error) {
	d := newRenderData(s, dnsIP, sec)
	out := map[string]string{}
	for path, t := range map[string]string{
		"install-config.yaml": "install-config.yaml.tmpl",
		"agent-config.yaml":   "agent-config.yaml.tmpl",
		"openshift/98-var-lib-containers-partitioned.yaml": "var-lib-containers.yaml.tmpl",
	} {
		y, err := render(t, d)
		if err != nil {
			return nil, err
		}
		out[path] = y
	}
	return out, nil
}

// Tar packs files for the builder script's stdin (see builder.Tar).
func Tar(files map[string]string) ([]byte, error) { return builder.Tar(files) }

// script returns an embedded builder script.
func script(name string) string {
	b, err := files.ReadFile("scripts/" + name)
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(b)
}
