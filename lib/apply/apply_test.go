package apply

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
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
