package guest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/release"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// GVRDataVolume is CDI's DataVolume.
var GVRDataVolume = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes"}

// Env is the cluster access and UI the guest commands need.
type Env struct {
	Kube     kubernetes.Interface
	Dyn      dynamic.Interface
	Applier  apply.Applier
	Builder  builder.Runner
	Resolver interface {
		Resolve(ctx context.Context, version string) (release.Release, error)
	}
	// Cache mirrors the release into the environment's registry so the guest
	// installs from it; nil pulls from the internet.
	Cache    Cache
	Progress precheck.Progress // nil = silent
	Poll     time.Duration     // 0 = 10s
	Timeout  time.Duration     // limit for an install; 0 = 90m
	Now      func() time.Time
}

func (e *Env) defaults() {
	if e.Progress == nil {
		e.Progress = precheck.NoProgress()
	}
	if e.Poll == 0 {
		e.Poll = 10 * time.Second
	}
	if e.Timeout == 0 {
		e.Timeout = 90 * time.Minute
	}
	if e.Now == nil {
		e.Now = time.Now
	}
}

// Guest is one vSNO as list shows it.
type Guest struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	State     State     `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	Version   string    `json:"version,omitempty"`
	Requested string    `json:"requested,omitempty"`
	Image     string    `json:"image,omitempty"`
	Node      string    `json:"node,omitempty"`
	Cores     int64     `json:"cores"`
	MemoryGiB int64     `json:"memoryGiB"`
	DiskGiB   int64     `json:"diskGiB"`
	ServiceIP string    `json:"serviceIP,omitempty"`
	API       string    `json:"api"`
	Created   time.Time `json:"created"`
	// Unhealthy counts ClusterOperators that are Degraded or not Available, once known.
	Unhealthy *int `json:"unhealthyOperators,omitempty"`
	// Cached is true when the guest installed from the registry mirror.
	Cached bool `json:"cached,omitempty"`
	// Timings are how long create's phases took; empty when not measured.
	Timings Timings `json:"timings,omitempty"`
}

// Timings are the durations create records as it goes.
type Timings struct {
	ISO       string `json:"iso,omitempty"`
	Install   string `json:"install,omitempty"`
	Reboot    string `json:"reboot,omitempty"`
	Available string `json:"available,omitempty"`
}

// Empty is true when nothing was measured, such as for a vSNO created without
// --watch.
func (t Timings) Empty() bool {
	return t == Timings{}
}

// ShownVersion is the installed version, or the requested one until it's known.
func (g Guest) ShownVersion() string {
	if g.Version != "" {
		return g.Version
	}
	return g.Requested
}

// API is the vSNO's API URL, as the guest and the builder name it.
func API(name string) string {
	return fmt.Sprintf("https://api.%s.%s:6443", name, manifests.DefaultBaseDomain)
}

// List returns the vSNOs in ns, sorted by name. Running vSNOs are probed
// through the builder in one call; if the builder isn't available their state
// is Unknown, with the reason in Detail.
func List(ctx context.Context, e Env, ns string) ([]Guest, error) {
	e.defaults()
	// Every VM in an environment's namespace is a vSNO. The vsno.io/name label
	// isn't required: an environment assembled by hand predates it.
	vms, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil // no KubeVirt
	}
	if err != nil {
		return nil, fmt.Errorf("listing vSNOs: %w", err)
	}
	out := make([]Guest, 0, len(vms.Items))
	obs := make([]Observed, 0, len(vms.Items))
	targets := map[string]string{}
	for i := range vms.Items {
		g, o := observe(ctx, e, &vms.Items[i])
		if o.VMStatus == "Running" && g.ServiceIP != "" {
			targets[g.Name] = g.ServiceIP
		}
		out, obs = append(out, g), append(obs, o)
	}
	probes, perr := Probes(ctx, e.Builder, targets)
	for i := range out {
		if p, ok := probes[out[i].Name]; ok {
			obs[i].Probe = p
		}
		out[i].State, out[i].Detail = Classify(obs[i])
		if out[i].State == Unknown && perr != nil && targets[out[i].Name] != "" {
			out[i].Detail = perr.Error()
		}
		if p := obs[i].Probe; p != nil {
			if p.CV != nil && p.CV.Version != "" {
				out[i].Version = p.CV.Version
			}
			out[i].Unhealthy = p.Unhealthy
		}
	}
	building, err := buildingISO(ctx, e, ns, out)
	if err != nil {
		return nil, err
	}
	out = append(out, building...)
	slices.SortFunc(out, func(a, b Guest) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Environments returns the namespaces that are vsno environments: those vsno
// labeled, plus any that hold the builder pod (an environment built by hand
// predates the label). Sorted.
func Environments(ctx context.Context, kube kubernetes.Interface) ([]string, error) {
	set := map[string]struct{}{}
	nss, err := kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: cluster.ManagedByLabel + "=vsno"})
	if err != nil {
		return nil, fmt.Errorf("listing the vsno environments: %w", err)
	}
	for _, n := range nss.Items {
		set[n.Name] = struct{}{}
	}
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: builder.LabelSelector})
	if err != nil {
		return nil, fmt.Errorf("finding the builders: %w", err)
	}
	for _, p := range pods.Items {
		set[p.Namespace] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// ListAll returns the vSNOs in every environment, sorted by namespace then
// name. envFor builds the Env for one namespace, so each environment's builder
// probes its own vSNOs.
func ListAll(ctx context.Context, kube kubernetes.Interface, envFor func(ns string) (Env, error)) ([]Guest, error) {
	envs, err := Environments(ctx, kube)
	if err != nil {
		return nil, err
	}
	var out []Guest
	for _, ns := range envs {
		e, err := envFor(ns)
		if err != nil {
			return nil, err
		}
		gs, err := List(ctx, e, ns)
		if err != nil {
			return nil, err
		}
		out = append(out, gs...)
	}
	slices.SortFunc(out, func(a, b Guest) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// buildingISO returns the vSNOs that have a Service but no VM yet: create
// adds the Service first, then builds the ISO, then makes the VM.
func buildingISO(ctx context.Context, e Env, ns string, have []Guest) ([]Guest, error) {
	svcs, err := e.Kube.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelName})
	if err != nil {
		return nil, fmt.Errorf("listing vSNO Services: %w", err)
	}
	var out []Guest
	for _, s := range svcs.Items {
		name := s.Labels[LabelName]
		if slices.ContainsFunc(have, func(g Guest) bool { return g.Name == name }) {
			continue
		}
		out = append(out, Guest{Namespace: ns, Name: name, State: Building, Requested: s.Annotations[AnnRequested],
			Version: s.Annotations[AnnVersion], Image: s.Annotations[AnnImage],
			ServiceIP: s.Spec.ClusterIP, API: API(name), Created: s.CreationTimestamp.Time})
	}
	return out, nil
}

// observe reads one vSNO's VM, ISO DataVolume, VMI and Service.
func observe(ctx context.Context, e Env, vm *unstructured.Unstructured) (Guest, Observed) {
	ns, name := vm.GetNamespace(), vm.GetName()
	ann := vm.GetAnnotations()
	g := Guest{Namespace: ns, Name: name, Requested: ann[AnnRequested], Image: ann[AnnImage], Version: ann[AnnVersion],
		API: API(name), Created: vm.GetCreationTimestamp().Time}
	g.Cores, _, _ = unstructured.NestedInt64(vm.Object, "spec", "template", "spec", "domain", "cpu", "cores")
	g.MemoryGiB = gib(nested(vm, "spec", "template", "spec", "domain", "memory", "guest"))
	if dvts, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates"); len(dvts) > 0 {
		if m, ok := dvts[0].(map[string]any); ok {
			s, _, _ := unstructured.NestedString(m, "spec", "storage", "resources", "requests", "storage")
			g.DiskGiB = gib(s)
		}
	}
	g.Node, _, _ = unstructured.NestedString(vm.Object, "spec", "template", "spec", "nodeSelector", "kubernetes.io/hostname")
	g.Cached = ann[AnnCached] == "true"
	g.Timings = Timings{ISO: ann[AnnTimingISO], Install: ann[AnnTimingInstall], Reboot: ann[AnnTimingReboot], Available: ann[AnnTimingAvailable]}

	o := Observed{VMStatus: nested(vm, "status", "printableStatus")}
	if dv, err := e.Dyn.Resource(GVRDataVolume).Namespace(ns).Get(ctx, name+"-iso", metav1.GetOptions{}); err == nil {
		o.ISOPhase, o.ISOProgress = nested(dv, "status", "phase"), nested(dv, "status", "progress")
	}
	if vmi, err := e.Dyn.Resource(cluster.GVRVMI).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		if n := nested(vmi, "status", "nodeName"); n != "" {
			g.Node = n
		}
	}
	if svc, err := e.Kube.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); err == nil && svc.Spec.ClusterIP != corev1.ClusterIPNone {
		g.ServiceIP = svc.Spec.ClusterIP
	}
	return g, o
}

func nested(u *unstructured.Unstructured, path ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, path...)
	return s
}

// gib parses a quantity such as "32Gi" into GiB; 0 if it doesn't parse.
func gib(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value() >> 30
}

// Probes runs probe.sh for targets (name → Service IP) in the builder.
func Probes(ctx context.Context, b builder.Runner, targets map[string]string) (map[string]*Probe, error) {
	if len(targets) == 0 || b == nil {
		return nil, nil
	}
	args := []string{manifests.DefaultBaseDomain}
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		args = append(args, n+"="+targets[n])
	}
	var stdout, stderr bytes.Buffer
	if err := b.Run(ctx, script("probe.sh"), args, builder.IO{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return nil, fmt.Errorf("probing through the builder: %w%s", err, tail(stderr.String()))
	}
	out := map[string]*Probe{}
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var p Probe
		if json.Unmarshal(sc.Bytes(), &p) == nil && p.Name != "" {
			out[p.Name] = &p
		}
	}
	return out, sc.Err()
}

// tail is the last line of s, as ": line", for error messages.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return ": " + s[strings.LastIndex(s, "\n")+1:]
}

// Capacity is what's left on the vSNO node.
type Capacity struct {
	Node          string `json:"node"`
	Cores         int64  `json:"cores"`         // allocatable CPUs: a vSNO may not have more
	FreeMemoryGiB int64  `json:"freeMemoryGiB"` // allocatable memory minus running pods' requests
	// FitDefault is how many more default-size vSNOs fit in FreeMemoryGiB.
	FitDefault int64 `json:"fitDefault"`
}

// vmOverheadGiB approximates the virt-launcher pod's memory beyond the guest's.
const vmOverheadGiB = 1

// NodeCapacity reports what's left on node, for vSNOs whose pods request
// their memory at percent overcommit (100 for none; see cluster.ReadDensity).
func NodeCapacity(ctx context.Context, kube kubernetes.Interface, node string, percent int) (Capacity, error) {
	n, err := kube.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return Capacity{}, fmt.Errorf("reading node %s: %w", node, err)
	}
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return Capacity{}, fmt.Errorf("listing pods on %s: %w", node, err)
	}
	free := n.Status.Allocatable.Memory().Value()
	for _, p := range pods.Items {
		if p.Spec.NodeName != node || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				free -= q.Value()
			}
		}
	}
	c := Capacity{Node: node, Cores: n.Status.Allocatable.Cpu().Value(), FreeMemoryGiB: max(0, free>>30)}
	c.FitDefault = c.FreeMemoryGiB / (cluster.RequestGiB(DefaultMemoryGiB, percent) + vmOverheadGiB)
	return c, nil
}
