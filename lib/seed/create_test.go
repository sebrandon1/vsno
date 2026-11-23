package seed

import (
	"context"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCreate(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[sgGet] = []resp{{Out: ""}} // nothing there yet
	fa.OcReplies[sgPoll] = []resp{{Out: sgRunningJS}, {Out: sgDoneJS}}
	e := world(t, fa)
	res, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantImage := Ref(testRegistry, "a", "4.20.46")
	if res.Image != wantImage || res.Version != "4.20.46" || res.LCA != "lifecycle-agent.v4.20.7" {
		t.Errorf("result = %+v", res)
	}
	if !fa.Ran("oc apply") || !strings.Contains(strings.Join(fa.Stdin, "\n"), wantImage) {
		t.Errorf("the SeedGenerator was not applied with the image: %q", fa.Stdin)
	}
	if !fa.Ran("oc delete seedgenerators seedimage") || !fa.Ran("oc delete secret seedgen") {
		t.Errorf("finish left the SeedGenerator behind: %q", fa.Calls)
	}
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := vm.GetAnnotations()[AnnSeedImage]; got != wantImage {
		t.Errorf("recorded seed = %q, want %q", got, wantImage)
	}
	if v, ok := vm.GetAnnotations()[guest.AnnSeeding]; ok {
		t.Errorf("the seeding mark stayed: %q", v)
	}
}

// A generation that fails leaves the vSNO usable: the mark is cleared, and the
// error says what LCA said.
func TestCreateClearsTheMarkWhenGenerationFails(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[sgGet] = []resp{{Out: ""}}
	fa.OcReplies[sgPoll] = []resp{{Out: sgFailedJS}}
	e := world(t, fa)
	res, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "a"})
	if err == nil || !strings.Contains(err.Error(), "push denied") {
		t.Fatalf("err = %v, want the generation failure", err)
	}
	if res.Image == "" {
		t.Errorf("result = %+v, want the image it tried", res)
	}
	vm, gerr := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if v, ok := vm.GetAnnotations()[guest.AnnSeeding]; ok {
		t.Errorf("the seeding mark stayed: %q", v)
	}
}

// A vSNO that isn't ready is refused by the precheck, before anything runs.
func TestCreateRefusesANotReadyVSNO(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess() // everything but the vSNO's state is fine
	e := world(t, fa)
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(vm.Object, "Stopped", "status", "printableStatus"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Update(ctx, vm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "a"}); err == nil || !strings.Contains(err.Error(), "a seed needs a finished install") {
		t.Errorf("err = %v, want the precheck's refusal", err)
	}
	if strings.Contains(strings.Join(fa.Stdin, "\n"), "seedImage") {
		t.Errorf("a seed generation was applied anyway: %q", fa.Stdin)
	}
}

// finish stops the vSNO when asked, and records that it did.
func TestCreateStopsTheVSNO(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[sgGet] = []resp{{Out: ""}}
	fa.OcReplies[sgPoll] = []resp{{Out: sgDoneJS}}
	e := world(t, fa)
	res, err := Create(ctx, e, CreateOptions{Namespace: ns, Name: "a", Stop: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped {
		t.Errorf("result = %+v, want Stopped", res)
	}
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running"); running {
		t.Errorf("the VM was not stopped: %v", vm.Object["spec"])
	}
}
