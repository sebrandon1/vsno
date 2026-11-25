package apply

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var (
	cmGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	nsGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

// resettableMapper counts Resets and only knows "late" kinds after one,
// like a CRD installed during setup.
type resettableMapper struct {
	*meta.DefaultRESTMapper
	late   []schema.GroupVersionKind
	resets int
}

func (m *resettableMapper) Reset() {
	m.resets++
	for _, gvk := range m.late {
		m.Add(gvk, meta.RESTScopeNamespace)
	}
}

func newMapper() *resettableMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	return &resettableMapper{DefaultRESTMapper: m}
}

// fakeApply makes the fake client treat apply patches as create-or-replace,
// bumping resourceVersion only when the content changes, like the API server.
func fakeApply(c *dynfake.FakeDynamicClient) {
	c.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.PatchAction)
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(p.GetPatch()); err != nil {
			return true, nil, err
		}
		tr := c.Tracker()
		cur, err := tr.Get(p.GetResource(), p.GetNamespace(), p.GetName())
		if err != nil {
			obj.SetResourceVersion("1")
			return true, obj, tr.Create(p.GetResource(), obj, p.GetNamespace())
		}
		old := cur.(*unstructured.Unstructured)
		rv := old.GetResourceVersion()
		old.SetResourceVersion("")
		if reflect.DeepEqual(old.Object, obj.Object) {
			old.SetResourceVersion(rv)
			return true, old, nil
		}
		n, _ := strconv.Atoi(rv)
		obj.SetResourceVersion(strconv.Itoa(n + 1))
		return true, obj, tr.Update(p.GetResource(), obj, p.GetNamespace())
	})
}

func newClient() *dynfake.FakeDynamicClient {
	c := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cmGVR: "ConfigMapList",
		nsGVR: "NamespaceList",
		{Group: "example.io", Version: "v1", Resource: "widgets"}: "WidgetList",
	})
	fakeApply(c)
	return c
}

func obj(kind, ns, name string, data map[string]any) *unstructured.Unstructured {
	apiVersion := "v1"
	if kind == "Widget" {
		apiVersion = "example.io/v1"
	}
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"name": name}}}
	if ns != "" {
		u.SetNamespace(ns)
	}
	if data != nil {
		u.Object["data"] = data
	}
	return u
}

func TestApplyReportsCreatedUpdatedUnchanged(t *testing.T) {
	ctx := context.Background()
	c := newClient()
	s := &SSA{Dyn: c, Mapper: newMapper()}

	objs := []*unstructured.Unstructured{obj("Namespace", "", "vsno", nil), obj("ConfigMap", "vsno", "a", map[string]any{"k": "1"})}
	ch, err := s.Apply(ctx, objs)
	if err != nil {
		t.Fatal(err)
	}
	if Count(ch, Created) != 2 {
		t.Fatalf("first apply: %v", ch)
	}

	ch, err = s.Apply(ctx, objs)
	if err != nil || Count(ch, Unchanged) != 2 {
		t.Fatalf("rerun should be a no-op: %v %v", ch, err)
	}

	objs[1] = obj("ConfigMap", "vsno", "a", map[string]any{"k": "2"})
	ch, err = s.Apply(ctx, objs)
	if err != nil || ch[1].Action != Updated || ch[0].Action != Unchanged {
		t.Fatalf("changed data should update: %v %v", ch, err)
	}
	if got := ch[1].String(); got != "vsno/ConfigMap/a updated" {
		t.Errorf("String() = %q", got)
	}
	if got := ch[0].Ref(); got != "Namespace/vsno" {
		t.Errorf("Ref() = %q", got)
	}

	// Field manager and force are sent.
	for _, a := range c.Actions() {
		if p, ok := a.(k8stesting.PatchActionImpl); ok {
			if p.PatchOptions.FieldManager != FieldManager || p.PatchOptions.Force == nil || !*p.PatchOptions.Force {
				t.Errorf("patch options = %+v", p.PatchOptions)
			}
		}
	}
}

func TestApplyResetsMapperForNewCRDs(t *testing.T) {
	m := newMapper()
	m.late = []schema.GroupVersionKind{{Group: "example.io", Version: "v1", Kind: "Widget"}}
	s := &SSA{Dyn: newClient(), Mapper: m}
	ch, err := s.Apply(context.Background(), []*unstructured.Unstructured{obj("Widget", "vsno", "w", nil)})
	if err != nil || len(ch) != 1 || ch[0].Action != Created {
		t.Fatalf("got %v %v", ch, err)
	}
	if m.resets != 1 {
		t.Errorf("resets = %d, want 1", m.resets)
	}
}

func TestApplyUnknownKind(t *testing.T) {
	s := &SSA{Dyn: newClient(), Mapper: newMapper()}
	_, err := s.Apply(context.Background(), []*unstructured.Unstructured{obj("Widget", "vsno", "w", nil)})
	if err == nil || !strings.Contains(err.Error(), "Widget") {
		t.Fatalf("want a no-match error naming the kind, got %v", err)
	}
}

func TestApplyStopsAtFirstError(t *testing.T) {
	c := newClient()
	c.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("denied")
	})
	c.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	s := &SSA{Dyn: c, Mapper: newMapper()}

	ch, err := s.Apply(context.Background(), []*unstructured.Unstructured{obj("ConfigMap", "vsno", "a", nil), obj("ConfigMap", "vsno", "b", nil)})
	if err == nil || !strings.Contains(err.Error(), "applying vsno/ConfigMap/a: denied") || len(ch) != 0 {
		t.Fatalf("got %v %v", ch, err)
	}
	_, err = s.Apply(context.Background(), []*unstructured.Unstructured{obj("Namespace", "", "vsno", nil)})
	if err == nil || !strings.Contains(err.Error(), "getting Namespace/vsno: api down") {
		t.Fatalf("a failed GET other than NotFound must stop: %v", err)
	}
}

func TestNewSSA(t *testing.T) {
	s := NewSSA(kubefake.NewClientset().Discovery(), newClient())
	if s.Dyn == nil || s.Mapper == nil {
		t.Fatal("NewSSA left a field nil")
	}
}

func TestPlan(t *testing.T) {
	ctx := context.Background()
	c := newClient()
	s := &SSA{Dyn: c, Mapper: newMapper()}
	ours := obj("ConfigMap", "vsno", "ours", nil)
	ours.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kube-controller-manager"}, {Manager: FieldManager}})
	theirs := obj("ConfigMap", "vsno", "theirs", nil)
	theirs.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}})
	for _, o := range []*unstructured.Unstructured{ours, theirs} {
		if _, err := c.Resource(cmGVR).Namespace("vsno").Create(ctx, o, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	ch, err := s.Plan(ctx, []*unstructured.Unstructured{
		obj("ConfigMap", "vsno", "new", nil),
		obj("ConfigMap", "vsno", "ours", nil),
		obj("ConfigMap", "vsno", "theirs", nil),
		obj("Widget", "vsno", "w", nil), // its CRD isn't installed yet
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []Action
	for _, x := range ch {
		got = append(got, x.Action)
	}
	if want := []Action{Create, Reapply, TakeOver, Create}; !reflect.DeepEqual(got, want) {
		t.Errorf("actions = %v, want %v", got, want)
	}
	for _, a := range c.Actions() {
		if v := a.GetVerb(); v != "get" && v != "create" {
			t.Errorf("Plan must only read, but did %s %s", v, a.GetResource().Resource)
		}
	}
}

func TestPlanErrors(t *testing.T) {
	c := newClient()
	c.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	s := &SSA{Dyn: c, Mapper: errMapper{}}
	if _, err := s.Plan(context.Background(), []*unstructured.Unstructured{obj("ConfigMap", "vsno", "a", nil)}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a mapping error other than no-match must stop: %v", err)
	}
	s.Mapper = newMapper()
	if _, err := s.Plan(context.Background(), []*unstructured.Unstructured{obj("ConfigMap", "vsno", "a", nil)}); err == nil || !strings.Contains(err.Error(), "getting vsno/ConfigMap/a: api down") {
		t.Errorf("a failed GET other than NotFound must stop: %v", err)
	}
}

type errMapper struct{}

func (errMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("boom")
}
func (errMapper) Reset() {}

func TestTally(t *testing.T) {
	ch := []Change{{Action: Create}, {Action: TakeOver}, {Action: Create}}
	if got := Tally(ch, "none", Label{Create, "to create"}, Label{Reapply, "to reapply"}, Label{TakeOver, "to take over"}); got != "2 to create, 1 to take over" {
		t.Errorf("Tally = %q", got)
	}
	if got := Tally(ch, "none", Label{Unchanged, "unchanged"}); got != "none" {
		t.Errorf("no labeled action: %q", got)
	}
}

func TestTakeOverReplacesWhenApplyIsRejected(t *testing.T) {
	// Server-side apply merges another manager's fields with ours, and the API
	// rejects the result when they conflict (two readinessProbe handlers, say).
	// Taking the object over replaces it instead.
	ctx := context.Background()
	c := newClient()
	s := &SSA{Dyn: c, Mapper: newMapper()}
	cur := obj("ConfigMap", "vsno", "theirs", map[string]any{"k": "old"})
	cur.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}, {Manager: "unrelated"}})
	if _, err := c.Resource(cmGVR).Namespace("vsno").Create(ctx, cur, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, invalidErr("merge rejected")
	})
	// A replace bumps the resourceVersion, as the API server does.
	c.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		u.SetResourceVersion("2")
		return true, u, c.Tracker().Update(cmGVR, u, u.GetNamespace())
	})
	ch, err := s.Apply(ctx, []*unstructured.Unstructured{obj("ConfigMap", "vsno", "theirs", map[string]any{"k": "new"})})
	if err != nil || len(ch) != 1 || ch[0].Action != Updated {
		t.Fatalf("%v %v", err, ch)
	}
	got, err := c.Resource(cmGVR).Namespace("vsno").Get(ctx, "theirs", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if k, _, _ := unstructured.NestedString(got.Object, "data", "k"); k != "new" {
		t.Errorf("data = %v", got.Object["data"])
	}

	// Our own object's rejected apply is still just an error: there is no
	// takeover to fall back to.
	c = newClient()
	s = &SSA{Dyn: c, Mapper: newMapper()}
	ours := obj("ConfigMap", "vsno", "ours", map[string]any{"k": "old"})
	ours.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: FieldManager}})
	if _, err := c.Resource(cmGVR).Namespace("vsno").Create(ctx, ours, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, invalidErr("merge rejected")
	})
	if _, err := s.Apply(ctx, []*unstructured.Unstructured{obj("ConfigMap", "vsno", "ours", nil)}); err == nil || !strings.Contains(err.Error(), "applying vsno/ConfigMap/ours: merge rejected") {
		t.Errorf("err = %v", err)
	}

	// A failed replace says it was taking the object over.
	c = newClient()
	s = &SSA{Dyn: c, Mapper: newMapper()}
	theirs := obj("ConfigMap", "vsno", "theirs", nil)
	theirs.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}})
	if _, err := c.Resource(cmGVR).Namespace("vsno").Create(ctx, theirs, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, invalidErr("merge rejected")
	})
	c.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("replace rejected")
	})
	if _, err := s.Apply(ctx, []*unstructured.Unstructured{obj("ConfigMap", "vsno", "theirs", nil)}); err == nil || !strings.Contains(err.Error(), "taking over vsno/ConfigMap/theirs: replace rejected") {
		t.Errorf("err = %v", err)
	}
}

func TestCarryOver(t *testing.T) {
	cur := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "registry", "namespace": "vsno", "resourceVersion": "7",
			"ownerReferences": []any{map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine", "name": "seed", "uid": "vm-1"}}},
		"spec": map[string]any{"clusterIP": "172.30.0.9", "clusterIPs": []any{"172.30.0.9"}, "type": "ClusterIP"},
	}}
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "registry", "namespace": "vsno"},
		"spec":     map[string]any{"ports": []any{}},
	}}
	carryOver(o, cur)
	ip, _, _ := unstructured.NestedString(o.Object, "spec", "clusterIP")
	ips, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "clusterIPs")
	if ip != "172.30.0.9" || len(ips) != 1 || o.GetResourceVersion() != "7" {
		t.Errorf("carried %v, rv %q", o.Object["spec"], o.GetResourceVersion())
	}
	if _, found, _ := unstructured.NestedString(o.Object, "spec", "type"); found {
		t.Error("carryOver copied a field the manifest sets")
	}
	// The template never renders ownerReferences (the VM's UID doesn't exist at
	// render time), so a replace must keep the reference that makes the Service
	// collect with its vSNO.
	refs := o.GetOwnerReferences()
	if len(refs) != 1 || refs[0].Name != "seed" || refs[0].UID != "vm-1" {
		t.Errorf("ownerReferences not carried: %+v", refs)
	}
}

// invalidErr is the 422 the API returns for a rejected object.
func invalidErr(msg string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonInvalid, Message: msg}}
}

func TestTakeOverOnlyReplacesOnInvalid(t *testing.T) {
	// A transient failure must not be papered over with a replace.
	ctx := context.Background()
	c := newClient()
	s := &SSA{Dyn: c, Mapper: newMapper()}
	cur := obj("ConfigMap", "vsno", "theirs", nil)
	cur.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}})
	if _, err := c.Resource(cmGVR).Namespace("vsno").Create(ctx, cur, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	if _, err := s.Apply(ctx, []*unstructured.Unstructured{obj("ConfigMap", "vsno", "theirs", nil)}); err == nil || !strings.Contains(err.Error(), "applying vsno/ConfigMap/theirs: api down") {
		t.Errorf("err = %v", err)
	}
	for _, a := range c.Actions() {
		if a.GetVerb() == "update" {
			t.Error("replaced the object after a non-422 error")
		}
	}
}
