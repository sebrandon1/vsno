package ibu

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/guestctl/guestctltest"
	"github.com/sebrandon1/vsno/lib/seed"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
)

const (
	ns       = "vsno-test"
	seedRef  = "registry.vsno-test.svc.cluster.local:5000/seeds/shiny-crane:4.21.42"
	ibuGet   = "get imagebasedupgrades upgrade --ignore-not-found --request-timeout=20s"
	lcaCSVs  = "get clusterserviceversions -n openshift-lifecycle-agent --ignore-not-found"
	oadpCSVs = "get clusterserviceversions -n openshift-adp --ignore-not-found"
	bslGet   = "get backupstoragelocations -n openshift-adp"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./lib/ibu -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file (go test ./lib/ibu -update):\n%s", name, got)
	}
}

func cond(t, status, reason, msg string) guestctl.Condition {
	return guestctl.Condition{Type: t, Status: status, Reason: reason, Message: msg}
}

func ibuWith(stage Stage, image string, conds ...guestctl.Condition) IBU {
	var u IBU
	u.Spec.Stage = stage
	u.Spec.SeedImageRef.Image = image
	u.Status.Conditions = conds
	return u
}

func TestToward(t *testing.T) {
	cases := []struct {
		name string
		u    IBU
		s    Stage
		want Progress
		msg  string
	}{
		{"idle arrived", ibuWith(StageIdle, "", cond("Idle", "True", "Idle", "Idle")), StageIdle, Arrived, "Idle"},
		{"idle not yet", ibuWith(StageIdle, "", cond("Idle", "False", "Finalizing", "finalizing")), StageIdle, Moving, ""},
		{"prep precaching", ibuWith(StagePrep, seedRef, cond("PrepInProgress", "True", "InProgress", "total: 4 (pulled: 1, failed: 0)")), StagePrep, Moving, "total: 4 (pulled: 1, failed: 0)"},
		{"prep done", ibuWith(StagePrep, seedRef, cond("PrepCompleted", "True", "Completed", "Prep stage completed successfully")), StagePrep, Arrived, "Prep stage completed successfully"},
		{"prep failed", ibuWith(StagePrep, seedRef, cond("PrepInProgress", "False", "Failed", "pull failed"), cond("PrepCompleted", "False", "Failed", "pull failed")), StagePrep, Stuck, "pull failed"},
		// As LCA reports it: the completed condition is generic, the in-progress one says why.
		{"prep failed, the reason on in-progress", ibuWith(StagePrep, seedRef, cond("PrepCompleted", "False", "Failed", "Prep failed"),
			cond("PrepInProgress", "False", "Failed", "failed to validate seed image info")), StagePrep, Stuck, "failed to validate seed image info"},
		{"failed with only the completed condition", ibuWith(StagePrep, seedRef, cond("PrepCompleted", "False", "Failed", "Prep failed")), StagePrep, Stuck, "Prep failed"},
		{"prep refused", ibuWith(StagePrep, seedRef, cond("PrepInProgress", "False", "InvalidTransition", "bad stage")), StagePrep, Stuck, "bad stage"},
		{"upgrade timed out", ibuWith(StageUpgrade, seedRef, cond("UpgradeCompleted", "False", "TimedOut", "too slow")), StageUpgrade, Stuck, "too slow"},
		{"upgrade blocked", ibuWith(StageUpgrade, seedRef, cond("UpgradeInProgress", "False", "Blocked", "IPC running")), StageUpgrade, Stuck, "IPC running"},
		{"nothing reported yet", ibuWith(StageUpgrade, seedRef), StageUpgrade, Moving, ""},
		// A completed condition left from a stage the spec has moved off isn't arrival.
		{"stale completion", ibuWith(StageUpgrade, seedRef, cond("PrepCompleted", "True", "Completed", "done")), StagePrep, Moving, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, msg := c.u.Toward(c.s); got != c.want || msg != c.msg {
				t.Errorf("Toward(%s) = %v %q, want %v %q", c.s, got, msg, c.want, c.msg)
			}
		})
	}
}

func TestPrecache(t *testing.T) {
	if p, f, n, ok := Precache("Prep in progress: total: 190 (pulled: 12, failed: 1)"); !ok || p != 12 || f != 1 || n != 190 {
		t.Errorf("= %d %d %d %v", p, f, n, ok)
	}
	for _, msg := range []string{"", "No precache status file to read yet.", "Stateroot setup in progress"} {
		if _, _, _, ok := Precache(msg); ok {
			t.Errorf("%q read as precache progress", msg)
		}
	}
}

func TestIdentityChanged(t *testing.T) {
	before := Identity{Version: "4.21.1", ClusterID: "c1", InfrastructureName: "i1", Node: "n", APIURL: "https://api"}
	after := before
	after.Version = "4.21.42"
	if got := before.Changed(after); len(got) != 0 {
		t.Errorf("a version change alone = %q", got)
	}
	after.ClusterID, after.Node = "c2", "m"
	if got := before.Changed(after); len(got) != 2 || got[0] != "clusterID c1 → c2" || got[1] != "node n → m" {
		t.Errorf("changed = %q", got)
	}
}

func TestResumeFrom(t *testing.T) {
	idle := ibuWith(StageIdle, "", cond("Idle", "True", "Idle", "Idle"))
	idle.Status.ValidNextStages = []Stage{StagePrep}
	cases := []struct {
		name    string
		u       IBU
		want    Stage
		wantErr string
	}{
		{"idle", idle, StagePrep, ""},
		{"new object", IBU{}, StagePrep, ""},
		{"prep with this seed", ibuWith(StagePrep, seedRef), StagePrep, ""},
		{"upgrade with this seed", ibuWith(StageUpgrade, seedRef), StageUpgrade, ""},
		{"prep with another seed", ibuWith(StagePrep, "other:1"), "", "with seed other:1"},
		{"rolling back", ibuWith(StageRollback, seedRef), "", "is in Rollback"},
	}
	idleNoPrep := idle
	idleNoPrep.Status.ValidNextStages = []Stage{StageRollback}
	cases = append(cases, struct {
		name    string
		u       IBU
		want    Stage
		wantErr string
	}{"idle refusing prep", idleNoPrep, "", "won't start Prep"})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resumeFrom(c.u, seedRef)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("= %s %v, want %s", got, err, c.want)
			}
		})
	}
}

func TestUpgradeOptionsCheck(t *testing.T) {
	ok := UpgradeOptions{Target: "t", Seed: "s", To: StageUpgrade}
	for _, c := range []struct {
		name string
		edit func(*UpgradeOptions)
		want string
	}{
		{"fine", func(*UpgradeOptions) {}, ""},
		{"no seed", func(o *UpgradeOptions) { o.Seed = "" }, "required"},
		{"bad stage", func(o *UpgradeOptions) { o.To = StageIdle }, "--to takes prep or upgrade"},
		{"both endings", func(o *UpgradeOptions) { o.Finalize, o.Rollback = true, true }, "opposites"},
		{"finalize after prep", func(o *UpgradeOptions) { o.To, o.Finalize = StagePrep, true }, "can't go with --to prep"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := ok
			c.edit(&o)
			err := o.check()
			if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestTemplates(t *testing.T) {
	for name, d := range map[string]data{
		"backup.yaml.tmpl":           {AccessKey: "k", SecretKey: "s", S3URL: "http://172.30.0.9:9000"},
		"seed-pull-secret.yaml.tmpl": {Auth: `{"auths":{"registry":{"auth":"dTpw"}}}`},
	} {
		y, err := render(name, d)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, strings.TrimSuffix(name, ".tmpl")+".golden", y)
	}
}

// lca simulates LCA's ImageBasedUpgrade on a target: a read returns the stage
// it was patched to, moving it along a step each time it is read. Everything
// else goes to the Fake, which answers the operators, identity and backups.
type lca struct {
	*guestctltest.Fake
	stage  Stage
	image  string
	reads  int // since the last patch
	reboot int // reads the API is down for after Upgrade or Rollback starts
	// prepFails and rollsBack make Prep fail, and Upgrade roll back on its own.
	prepFails, rollsBack bool
	patches              []string
}

func (l *lca) Oc(ctx context.Context, args []string, s builder.IO) error {
	key := strings.TrimSuffix(strings.Join(args, " "), " -o json")
	switch {
	case key == ibuGet:
		return l.read(s)
	case strings.HasPrefix(key, "patch imagebasedupgrades upgrade"):
		p := args[len(args)-1]
		l.patches = append(l.patches, p)
		var spec struct {
			Spec struct {
				Stage        Stage `json:"stage"`
				SeedImageRef struct {
					Image string `json:"image"`
				} `json:"seedImageRef"`
			} `json:"spec"`
		}
		_ = json.Unmarshal([]byte(p), &spec)
		l.stage, l.reads = spec.Spec.Stage, 0
		if spec.Spec.SeedImageRef.Image != "" {
			l.image = spec.Spec.SeedImageRef.Image
		}
		return nil
	}
	return l.Fake.Oc(ctx, args, s)
}

func (l *lca) read(s builder.IO) error {
	l.reads++
	if (l.stage == StageUpgrade || l.stage == StageRollback) && l.reads <= l.reboot {
		return errors.New("connection refused")
	}
	var u IBU
	switch {
	case l.stage == "" || l.stage == StageIdle:
		u = ibuWith(StageIdle, "", cond("Idle", "True", "Idle", "Idle"))
		u.Status.ValidNextStages = []Stage{StagePrep}
	case l.stage == StagePrep && l.prepFails:
		u = ibuWith(StagePrep, l.image, cond("PrepInProgress", "False", "Failed", "failed to pull the seed image"))
	case l.stage == StagePrep && l.reads < 3:
		u = ibuWith(StagePrep, l.image, cond("PrepInProgress", "True", "InProgress", "total: 4 (pulled: 2, failed: 0)"))
	case l.stage == StagePrep:
		u = ibuWith(StagePrep, l.image, cond("PrepCompleted", "True", "Completed", "Prep completed"))
	case l.stage == StageUpgrade && l.rollsBack:
		u = ibuWith(StageRollback, l.image, cond("RollbackInProgress", "True", "InProgress", "lca-init-monitor timed out"))
	case l.stage == StageUpgrade:
		u = ibuWith(StageUpgrade, l.image, cond("UpgradeCompleted", "True", "Completed", "Upgrade completed"))
	case l.stage == StageRollback:
		u = ibuWith(StageRollback, l.image, cond("RollbackCompleted", "True", "Completed", "Rollback completed"))
	}
	u.Status.History = []History{{Stage: StagePrep, StartTime: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}}
	b, _ := json.Marshal(u)
	_, _ = s.Stdout.Write(b)
	return nil
}

// identityReplies answer the identity reads: version before, then after.
func identityReplies(f *guestctltest.Fake) {
	f.OcReplies["get clusterversion version"] = []guestctltest.Reply{
		{Out: `{"spec":{"clusterID":"c1"},"status":{"desired":{"version":"4.21.1"}}}`},
		{Out: `{"spec":{"clusterID":"c1"},"status":{"desired":{"version":"4.21.42"}}}`},
	}
	f.OcReplies["get infrastructure cluster"] = []guestctltest.Reply{{Out: `{"status":{"infrastructureName":"i1","apiServerURL":"https://api.t.vsno.lab:6443"}}`}}
	f.OcReplies["get nodes"] = []guestctltest.Reply{{Out: `{"items":[{"metadata":{"name":"t"}}]}`}}
}

// targetFake is a target with LCA and OADP installed and its backup location
// Available.
func targetFake() *guestctltest.Fake {
	f := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{
		lcaCSVs:  {{Out: `{"items":[{"metadata":{"name":"lifecycle-agent.v4.21.5"},"status":{"phase":"Succeeded"}}]}`}},
		oadpCSVs: {{Out: `{"items":[{"metadata":{"name":"oadp-operator.v1.5.2"},"status":{"phase":"Succeeded"}}]}`}},
		bslGet:   {{Out: `{"items":[{"metadata":{"name":"default"},"status":{"phase":"Available"}}]}`}},
	}}
	identityReplies(f)
	return f
}

// listing answers registry.sh list with the environment's seed images.
type listing struct{ out string }

func (l listing) Run(_ context.Context, _ string, args []string, s builder.IO) error {
	if len(args) > 1 && args[1] == "list" {
		_, _ = io.WriteString(s.Stdout, l.out)
	}
	return nil
}

const seedListing = `{"repo":"seeds/shiny-crane","tag":"4.21.42","digest":"sha256:a","created":"2026-11-19T23:00:00Z","labels":{"com.openshift.lifecycle-agent.seed_cluster_info":"{\"seed_cluster_ocp_version\":\"4.21.42\"}"},"size":1}
{"repo":"seeds/shiny-crane","tag":"4.21.1","digest":"sha256:b","created":"2026-11-18T23:00:00Z","labels":{"com.openshift.lifecycle-agent.seed_cluster_info":"{\"seed_cluster_ocp_version\":\"4.21.1\"}"},"size":1}
`

// testEnv is an environment whose registry and S3 exist, whose vSNOs answer
// with access, and whose registry lists the shiny-crane seeds.
// targetVM is the vSNO's VM, which an upgrade records its new version on.
func targetVM() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	u.SetAPIVersion("kubevirt.io/v1")
	u.SetKind("VirtualMachine")
	u.SetNamespace(ns)
	u.SetName("t")
	return u
}

func testEnv(access map[string]guestctl.Access) Env {
	objs := []runtime.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-auth"}, Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "registry-ca"}, Data: map[string][]byte{"ca.crt": []byte("CA")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "s3-config"}, Data: map[string][]byte{"accessKey": []byte("k"), "secretKey": []byte("s")}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "s3"}, Spec: corev1.ServiceSpec{ClusterIP: "172.30.0.9", Ports: []corev1.ServicePort{{Port: 9000}}}},
	}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	return Env{
		Env: guest.Env{Kube: kfake.NewClientset(objs...), Dyn: dynfake.NewSimpleDynamicClient(runtime.NewScheme(), targetVM()), Builder: listing{out: seedListing}, Poll: time.Millisecond,
			Now: func() time.Time { now = now.Add(time.Second); return now }},
		Access: func(name string) seed.Access {
			if a, ok := access[name]; ok {
				return a
			}
			return &guestctltest.Fake{} // a vSNO that answers nothing
		},
	}
}

var deleteOptions = metav1.DeleteOptions{}
