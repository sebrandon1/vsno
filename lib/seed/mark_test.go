package seed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/precheck"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

// markedEnv is a vSNO with (or without) the seeding mark, and a fake for
// whatever the guest answers about its SeedGenerator.
func markedEnv(t *testing.T, withMark bool, fa *fakeAccess) (Env, *dynfake.FakeDynamicClient) {
	t.Helper()
	ann := map[string]any{}
	if withMark {
		ann[guest.AnnSeeding] = "2026-11-19T11:59:00Z"
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": "seed", "namespace": ns, "annotations": ann},
	}}
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cluster.GVRVirtualMachine: "VirtualMachineList",
	}, u)
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	e := Env{Env: guest.Env{Dyn: d, Now: func() time.Time { return at }, Progress: precheck.NoProgress(), Poll: time.Millisecond},
		Access: func(string) Access { return fa }}
	e.Defaults(ns)
	return e, d
}

func markOf(t *testing.T, d *dynfake.FakeDynamicClient) string {
	t.Helper()
	vm, err := d.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "seed", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return vm.GetAnnotations()[guest.AnnSeeding]
}

func TestClearEndedMark(t *testing.T) {
	ctx := context.Background()

	// Nothing to clear: not an error, and not "cleared".
	e, _ := markedEnv(t, false, &fakeAccess{})
	if cleared, err := ClearEndedMark(ctx, e, ns, "seed"); err != nil || cleared {
		t.Errorf("no mark: cleared=%v err=%v", cleared, err)
	}

	// No SeedGenerator to read is a generation that ended.
	e, d := markedEnv(t, true, &fakeAccess{})
	cleared, err := ClearEndedMark(ctx, e, ns, "seed")
	if err != nil || !cleared {
		t.Fatalf("cleared=%v err=%v", cleared, err)
	}
	if markOf(t, d) != "" {
		t.Error("the mark is still there")
	}

	// A guest that cannot be read keeps the mark: seedGenEnded cannot tell.
	fa := &fakeAccess{OcReplies: map[string][]resp{
		"get seedgenerators seedimage --ignore-not-found --request-timeout=15s": {{Err: errors.New("guest is down")}},
	}}
	e, d = markedEnv(t, true, fa)
	if cleared, err := ClearEndedMark(ctx, e, ns, "seed"); err != nil || cleared {
		t.Errorf("unreadable guest: cleared=%v err=%v", cleared, err)
	}
	if markOf(t, d) == "" {
		t.Error("the mark went while the generation could not be checked")
	}
}

func TestClearEndedMarkWithoutTheVM(t *testing.T) {
	e, _ := markedEnv(t, false, &fakeAccess{})
	if _, err := ClearEndedMark(context.Background(), e, ns, "ghost"); err == nil {
		t.Error("want an error for a vSNO that does not exist")
	}
}
