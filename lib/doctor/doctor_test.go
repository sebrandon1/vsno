package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/guestctl/guestctltest"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
)

const ns = "vsno-test"

// fake builds an Env whose one vSNO answers with the given oc and ssh replies.
func fake(oc map[string][]guestctltest.Reply, ssh map[string][]guestctltest.Reply) (Env, *guestctltest.Fake) {
	f := &guestctltest.Fake{OcReplies: oc, SSHReplies: ssh}
	e := Env{}
	e.Env = seed.Env{Env: guest.Env{Kube: kfake.NewClientset()}, Access: func(string) seed.Access { return f }}
	e.Guests = func(context.Context, string) ([]guest.Guest, error) {
		return []guest.Guest{{Name: "a", State: guest.Ready}}, nil
	}
	return e, f
}

func TestHealthy(t *testing.T) {
	e, _ := fake(nil, nil)
	got, err := Run(context.Background(), e, ns, nil)
	if err != nil || len(got) != 0 {
		t.Errorf("findings = %+v, %v", got, err)
	}
}

// The Idle condition stuck at a reason LCA does not handle is named, and --fix
// points it at the one it does.
func TestStuckIdleCondition(t *testing.T) {
	stuck := `{"spec":{"stage":"Prep"},"status":{"conditions":[{"type":"Idle","status":"False","reason":"InProgress"},{"type":"PrepCompleted","status":"False","reason":"Failed"}]}}`
	e, f := fake(map[string][]guestctltest.Reply{
		"get imagebasedupgrades upgrade -n " + seed.LCANamespace + " --ignore-not-found": {{Out: stuck}},
	}, nil)
	got, err := Run(context.Background(), e, ns, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("findings = %+v, %v", got, err)
	}
	if got[0].Status != precheck.Fail || !strings.Contains(got[0].Detail, "InProgress") || got[0].Action != UnstickIdle {
		t.Errorf("finding = %+v", got[0])
	}
	if err := Fix(context.Background(), f, got[0]); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(f.Calls, "\n")
	if !strings.Contains(calls, `"path":"/status/conditions/0/reason"`) || !strings.Contains(calls, `"value":"Aborting"`) {
		t.Errorf("calls = %q", calls)
	}
	if !strings.Contains(calls, "--subresource=status") {
		t.Errorf("the patch was not a status patch: %q", calls)
	}
}

// A condition LCA is working on is not a finding.
func TestAbortingIsNotAFinding(t *testing.T) {
	busy := `{"spec":{"stage":"Prep"},"status":{"conditions":[{"type":"Idle","status":"False","reason":"Aborting"}]}}`
	e, _ := fake(map[string][]guestctltest.Reply{
		"get imagebasedupgrades upgrade -n " + seed.LCANamespace + " --ignore-not-found": {{Out: busy}},
	}, nil)
	if got, err := Run(context.Background(), e, ns, nil); err != nil || len(got) != 0 {
		t.Errorf("findings = %+v, %v", got, err)
	}
}

// A seed generation nothing follows is reported, and it is not --fix's job.
func TestSeedGenerationNotFollowed(t *testing.T) {
	sg := `{"status":{"conditions":[{"type":"SeedGenInProgress","status":"True","message":"Pulling recert image"}]}}`
	e, _ := fake(map[string][]guestctltest.Reply{
		"get seedgenerators seedimage -n " + seed.LCANamespace + " --ignore-not-found": {{Out: sg}},
	}, nil)
	got, err := Run(context.Background(), e, ns, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("findings = %+v, %v", got, err)
	}
	if got[0].Status != precheck.Warn || got[0].Action != "" || !strings.Contains(got[0].Detail, "Pulling recert image") {
		t.Errorf("finding = %+v", got[0])
	}
}

// The bricked-vSNO state: the orchestration directory is there and the
// services are off. --fix turns them back on.
func TestSeedGenLeftServicesDisabled(t *testing.T) {
	sshKey := "test -d /var/lib/lca/ibu-seedgen-orch && echo orch ; systemctl is-enabled kubelet crio"
	e, f := fake(nil, map[string][]guestctltest.Reply{sshKey: {{Out: "orch\ndisabled\ndisabled\n"}}})
	got, err := Run(context.Background(), e, ns, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("findings = %+v, %v", got, err)
	}
	if got[0].Action != EnableServices || !strings.Contains(got[0].Hint, "systemctl enable --now crio kubelet") {
		t.Errorf("finding = %+v", got[0])
	}
	if err := Fix(context.Background(), f, got[0]); err != nil {
		t.Fatal(err)
	}
	if !f.Ran("ssh sudo systemctl enable --now crio kubelet") {
		t.Errorf("calls = %q", f.Calls)
	}
}

// Services enabled again, or no orchestration directory, is not a finding.
func TestSeedGenServicesHealthy(t *testing.T) {
	sshKey := "test -d /var/lib/lca/ibu-seedgen-orch && echo orch ; systemctl is-enabled kubelet crio"
	for _, out := range []string{"orch\nenabled\nenabled\n", "disabled\ndisabled\n"} {
		e, _ := fake(nil, map[string][]guestctltest.Reply{sshKey: {{Out: out}}})
		if got, err := Run(context.Background(), e, ns, nil); err != nil || len(got) != 0 {
			t.Errorf("%q: findings = %+v, %v", out, got, err)
		}
	}
}

// A Released volume and a registry nearly full are the environment's findings,
// and neither is --fix's job.
func TestEnvironmentChecks(t *testing.T) {
	e, _ := fake(nil, nil)
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Namespace: ns, Name: "target-root"},
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("200Gi")},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.PersistentVolumePhase("Released")},
	}
	if err := e.Kube.(*kfake.Clientset).Tracker().Add(pv); err != nil {
		t.Fatal(err)
	}
	e.Volume = func(context.Context, string) (int64, int64, error) { return 99 << 30, 100 << 30, nil }
	got, err := Run(context.Background(), e, ns, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("findings = %+v, %v", got, err)
	}
	byCheck := map[string]Finding{}
	for _, f := range got {
		byCheck[f.Check] = f
	}
	if room, ok := byCheck["registry space"]; !ok || room.Status != precheck.Fail || room.Action != "" {
		t.Errorf("registry space = %+v", byCheck["registry space"])
	}
	if leak, ok := byCheck["storage"]; !ok || leak.Status != precheck.Warn {
		t.Errorf("storage = %+v", byCheck["storage"])
	}

	// Naming vSNOs skips the environment's own checks.
	if got, err := Run(context.Background(), e, ns, []string{"a"}); err != nil || len(got) != 0 {
		t.Errorf("named: %+v, %v", got, err)
	}
}

// fix refuses a finding it has no action for.
func TestFixRefusesTheRest(t *testing.T) {
	e, f := fake(nil, nil)
	_ = e
	if err := Fix(context.Background(), f, Finding{Result: precheck.Result{Check: "storage"}}); err == nil || !strings.Contains(err.Error(), "no automatic fix") {
		t.Errorf("err = %v", err)
	}
}
