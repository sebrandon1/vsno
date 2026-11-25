package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/release"
	"github.com/sebrandon1/vsno/lib/state"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
)

const (
	ns   = "vsno-test"
	node = "worker-1"
)

// call is one builder script run.
type call struct {
	script string // the script's file name, or "inline"
	args   []string
	stdin  string
}

// fakeRunner answers builder scripts with canned output, by script name.
type fakeRunner struct {
	calls   []call
	outputs map[string]string // script → stdout
	errs    map[string]error  // script → error
}

func scriptName(s string) string {
	for _, n := range []string{"iso.sh", "probe.sh", "rm.sh"} {
		if s == script(n) {
			return n
		}
	}
	return "inline"
}

func (f *fakeRunner) Run(_ context.Context, s string, args []string, streams builder.IO) error {
	c := call{script: scriptName(s), args: args}
	if streams.Stdin != nil {
		b, _ := io.ReadAll(streams.Stdin)
		c.stdin = string(b)
	}
	f.calls = append(f.calls, c)
	if streams.Stdout != nil {
		_, _ = io.WriteString(streams.Stdout, f.outputs[c.script])
	}
	if err := f.errs[c.script]; err != nil {
		if streams.Stderr != nil {
			_, _ = io.WriteString(streams.Stderr, "some detail\nlast line")
		}
		return err
	}
	return nil
}

func (f *fakeRunner) ran(script string) []call {
	var out []call
	for _, c := range f.calls {
		if c.script == script {
			out = append(out, c)
		}
	}
	return out
}

// tracker applies objects into the fake dynamic client, like SSA would.
type tracker struct{ dyn *dynfake.FakeDynamicClient }

var gvrs = map[string]schema.GroupVersionResource{
	"Service": cluster.GVRService, "DataVolume": GVRDataVolume, "VirtualMachine": cluster.GVRVirtualMachine,
}

func (t tracker) Apply(_ context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	var ch []apply.Change
	for _, o := range objs {
		o = o.DeepCopy()
		// The API server assigns a UID; the fake tracker doesn't.
		if o.GetUID() == "" {
			o.SetUID(types.UID("uid-" + o.GetName()))
		}
		if err := t.dyn.Tracker().Create(gvrs[o.GetKind()], o, o.GetNamespace()); err != nil {
			return ch, err
		}
		ch = append(ch, apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), Action: apply.Created})
	}
	return ch, nil
}

type fakeResolver struct{ err error }

func (f fakeResolver) Resolve(_ context.Context, v string) (release.Release, error) {
	switch {
	case f.err != nil:
		return release.Release{}, f.err
	case strings.Contains(v, "nightly"):
		return release.Release{Requested: v, Version: "4.22.0-0.nightly-2026-11-10-072952", Image: "registry.ci.openshift.org/ocp/release:4.22.0-0.nightly-2026-11-10-072952", Nightly: true}, nil
	case strings.Contains(v, "/"):
		return release.Release{Requested: v, Image: v}, nil
	}
	return release.Release{Requested: v, Version: "4.20.46", Image: "quay.io/openshift-release-dev/ocp-release:4.20.46-x86_64"}, nil
}

// world is a fake cluster with an environment vsno set up.
type world struct {
	kube   *kfake.Clientset
	dyn    *dynfake.FakeDynamicClient
	runner *fakeRunner
}

func newWorld(t *testing.T, objs ...runtime.Object) *world {
	t.Helper()
	ready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "builder-1", Labels: map[string]string{"app": "builder"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("90Gi"), corev1.ResourceCPU: resource.MustParse("104")}}}
	w := &world{
		kube: kfake.NewClientset(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{cluster.ManagedByLabel: "vsno"}}},
			ready, n,
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "dnsmasq"}, Spec: corev1.ServiceSpec{ClusterIP: "172.30.0.53"}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-config", Name: "pull-secret"},
				Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths": {"quay.io": {"auth": "x"}, "registry.ci.openshift.org": {"auth": "y"}}}`)}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "vsno-ssh"}, Data: map[string][]byte{"id_ed25519.pub": []byte("ssh-ed25519 AAAAtest vsno\n")}},
		),
		dyn: dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			cluster.GVRVirtualMachine: "VirtualMachineList", GVRDataVolume: "DataVolumeList", cluster.GVRVMI: "VirtualMachineInstanceList",
		}, objs...),
		runner: &fakeRunner{outputs: map[string]string{"iso.sh": "extracting openshift-install\nversion 4.20.46\nISO ready\n"}, errs: map[string]error{}},
	}
	if err := state.Save(context.Background(), w.kube, ns, &state.State{Node: node}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) env() Env {
	return Env{Kube: w.kube, Dyn: w.dyn, Applier: tracker{w.dyn}, Builder: w.runner, Resolver: fakeResolver{},
		Progress: precheck.NoProgress(), Now: time.Now, Poll: time.Millisecond, Timeout: time.Second}
}

// vm is a vSNO VM as KubeVirt reports it.
func vm(name, printable string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{LabelName: name},
			"annotations":       map[string]any{AnnRequested: "4.20", AnnVersion: "4.20.46", AnnImage: "quay.io/x:4.20.46"},
			"creationTimestamp": "2026-11-19T10:00:00Z"},
		"spec": map[string]any{
			"dataVolumeTemplates": []any{map[string]any{"spec": map[string]any{"storage": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": "200Gi"}}}}}},
			"template": map[string]any{"spec": map[string]any{
				"nodeSelector": map[string]any{"kubernetes.io/hostname": node},
				"domain":       map[string]any{"cpu": map[string]any{"cores": int64(16)}, "memory": map[string]any{"guest": "32Gi"}}}},
		},
		"status": map[string]any{"printableStatus": printable},
	}}
	return u
}

func dv(name, phase, progress string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"status":   map[string]any{"phase": phase, "progress": progress},
	}}
}

func svc(name, ip string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{ClusterIP: ip}}
}

func probeLine(name string, p string) string { return fmt.Sprintf(`{"name":%q,%s}`, name, p) + "\n" }

var errBoom = errors.New("boom")

// dnsKeys returns the keys of the guests ConfigMap.
func (w *world) dnsKeys(t *testing.T) map[string]string {
	t.Helper()
	cm, err := w.kube.CoreV1().ConfigMaps(ns).Get(context.Background(), manifests.GuestsConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return cm.Data
}
