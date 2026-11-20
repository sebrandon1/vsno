package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

func hyperConverged(spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetAPIVersion("hco.kubevirt.io/v1beta1")
	u.SetKind("HyperConverged")
	u.SetNamespace(CNVNamespace)
	u.SetName(HyperConvergedName)
	return u
}

func TestReadDensity(t *testing.T) {
	ctx := context.Background()
	onNode := map[string]any{"nodeLabelSelector": map[string]any{"matchLabels": map[string]any{hostnameLabel: "worker-0"}}}
	for _, c := range []struct {
		name string
		objs []runtime.Object
		want state.Density
	}{
		{"no HyperConverged", nil, state.Density{}},
		{"nothing set", []runtime.Object{hyperConverged(map[string]any{})}, state.Density{}},
		{"overcommit", []runtime.Object{hyperConverged(map[string]any{"higherWorkloadDensity": map[string]any{"memoryOvercommitPercentage": int64(150)}})}, state.Density{OvercommitPercent: 150}},
		{"KSM on one node", []runtime.Object{hyperConverged(map[string]any{"ksmConfiguration": onNode})}, state.Density{KSMNode: "worker-0"}},
		{"KSM everywhere", []runtime.Object{hyperConverged(map[string]any{"ksmConfiguration": map[string]any{}})}, state.Density{KSMNode: "*"}},
	} {
		got, err := ReadDensity(ctx, fakeDyn(c.objs...))
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	dyn := fakeDyn()
	dyn.PrependReactor("get", "hyperconvergeds", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("down") })
	if _, err := ReadDensity(ctx, dyn); err == nil || !strings.Contains(err.Error(), "reading HyperConverged: down") {
		t.Errorf("err = %v", err)
	}
}

// ApplyDensity applies only the density fields, as its own manager, so
// setup's own apply of HyperConverged leaves them alone.
func TestApplyDensity(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		d    state.Density
		want string
	}{
		{state.Density{}, `{}`},
		{state.Density{OvercommitPercent: 150}, `{"higherWorkloadDensity":{"memoryOvercommitPercentage":150}}`},
		{state.Density{KSMNode: "*"}, `{"ksmConfiguration":{}}`},
		{state.Density{KSMNode: "worker-0"}, `{"ksmConfiguration":{"nodeLabelSelector":{"matchLabels":{"kubernetes.io/hostname":"worker-0"}}}}`},
	} {
		dyn := fakeDyn()
		var spec, manager string
		dyn.PrependReactor("patch", "hyperconvergeds", func(a k8stesting.Action) (bool, runtime.Object, error) {
			p := a.(k8stesting.PatchAction)
			if p.GetPatchType() != types.ApplyPatchType {
				t.Errorf("patch type = %s", p.GetPatchType())
			}
			var obj map[string]any
			if err := json.Unmarshal(p.GetPatch(), &obj); err != nil {
				return true, nil, err
			}
			b, _ := json.Marshal(obj["spec"])
			spec = string(b)
			if pa, ok := a.(k8stesting.PatchActionImpl); ok {
				manager = pa.PatchOptions.FieldManager
			}
			return true, hyperConverged(nil), nil
		})
		if err := ApplyDensity(ctx, dyn, c.d); err != nil || spec != c.want || manager != DensityManager {
			t.Errorf("%+v: spec %s, manager %q, %v; want %s", c.d, spec, manager, err, c.want)
		}
	}
	dyn := fakeDyn()
	dyn.PrependReactor("patch", "hyperconvergeds", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("down") })
	if err := ApplyDensity(ctx, dyn, state.Density{KSMNode: "*"}); err == nil || !strings.Contains(err.Error(), "setting HyperConverged's density") {
		t.Errorf("err = %v", err)
	}
}

func TestRequestGiB(t *testing.T) {
	for _, c := range []struct {
		guest   int64
		percent int
		want    int64
	}{{16, 0, 16}, {16, 100, 16}, {16, 150, 11}, {16, 200, 8}, {1, 1000, 1}} {
		if got := RequestGiB(c.guest, c.percent); got != c.want {
			t.Errorf("RequestGiB(%d, %d) = %d, want %d", c.guest, c.percent, got, c.want)
		}
	}
}
