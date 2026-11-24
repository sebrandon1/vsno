package seed

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/precheck"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	cosGet  = "get clusteroperators"
	mcpsGet = "get machineconfigpools"
	imgGet  = "get image.config.openshift.io cluster"
	imgOurs = `{"spec":{"additionalTrustedCA":{"name":"` + trustConfigMap + `"}}}`
	imgNone = `{"spec":{}}`
	imgElse = `{"spec":{"additionalTrustedCA":{"name":"corp-ca"}}}`
)

// certTest is the ssh check ensureTrust makes for the registry's CA.
var certTest = "test -f " + certFile(testRegistry)

// readyAccess answers as a vSNO that needs nothing: settled, LCA and OADP
// installed, trusting the registry, and the node holding the CA.
func readyAccess() *fakeAccess {
	return &fakeAccess{
		OcReplies: map[string][]resp{
			cosGet:     {{Out: cosStable}},
			mcpsGet:    {{Out: mcpsStable}},
			csvGet:     {{Out: csvDone}},
			oadpCSVGet: {{Out: oadpDone}},
			imgGet:     {{Out: imgOurs}},
		},
		SSHReplies: map[string][]resp{certTest: {{}}},
	}
}

// inspect runs the checks for a ready vSNO with fa's answers.
func inspect(t *testing.T, fa *fakeAccess, o PrecheckOptions) (*inspector, *PrecheckReport) {
	t.Helper()
	g := guest.Guest{Namespace: ns, Name: "a", State: guest.Ready, Version: "4.20.46", Cores: 16, MemoryGiB: 32, DiskGiB: 200}
	rep := &PrecheckReport{VSNO: g.Name, Version: g.ShownVersion(), Image: Ref(testRegistry, g.Name, g.ShownVersion()), Registry: testRegistry}
	in := &inspector{Env: testEnv(fa), o: o, g: &g, a: fa, reg: testRegistry, rep: rep}
	in.run(context.Background())
	return in, rep
}

func rowOf(t *testing.T, rep *PrecheckReport, check string) precheck.Result {
	t.Helper()
	for _, res := range rep.Results {
		if res.Check == check {
			return res
		}
	}
	t.Fatalf("no %q row in %+v", check, rep.Results)
	return precheck.Result{}
}

// checkCase is one answer set for the checks, and the row it should produce.
type checkCase struct {
	name   string
	mutate func(*fakeAccess)
	check  string
	want   precheck.Status
	detail string
	hint   string
}

var checkCases = []checkCase{
	{"everything in place", func(*fakeAccess) {}, "Seed target", precheck.Pass, Ref(testRegistry, "a", "4.20.46"), ""},
	{"LCA installing", func(f *fakeAccess) { f.OcReplies[csvGet] = []resp{{Out: csvPending}} },
		"Lifecycle Agent", precheck.Warn, "not Succeeded yet", "wait for it"},
	{"LCA missing", func(f *fakeAccess) {
		f.OcReplies[csvGet] = []resp{{Out: ""}}
		f.OcReplies[pmGet] = []resp{{Out: `{"status":{"catalogSource":"redhat-operators","defaultChannel":"stable"}}`}}
	}, "Lifecycle Agent", precheck.Fail, "not installed", "--install subscribes to redhat-operators/stable"},
	// vsno prepare --lca-ref deploys LCA without OLM: no CSV, no olm.owner label.
	{"LCA from source", func(f *fakeAccess) {
		f.OcReplies[csvGet] = []resp{{Out: ""}}
		f.OcReplies["get deployments "+LCADeployment+" -n "+LCANamespace+" --ignore-not-found"] = []resp{{Out: `{"metadata":{"labels":{}},` +
			`"spec":{"template":{"spec":{"containers":[{"name":"manager","image":"registry.example:5000/lca:abc"}]}}}}`}}
	}, "Lifecycle Agent", precheck.Pass, "deployed from source: registry.example:5000/lca:abc", ""},
	{"LCA package missing", func(f *fakeAccess) { f.OcReplies[csvGet] = []resp{{Out: ""}} },
		"Lifecycle Agent", precheck.Fail, "no lifecycle-agent package", "expected for a nightly"},
	{"trust not set up", func(f *fakeAccess) { f.OcReplies[imgGet] = []resp{{Out: imgNone}} },
		"Registry trust", precheck.Fail, "no additionalTrustedCA", "--install adds ConfigMap " + trustConfigMap},
	{"someone else's trust", func(f *fakeAccess) { f.OcReplies[imgGet] = []resp{{Out: imgElse}} },
		"Registry trust", precheck.Fail, "openshift-config/corp-ca", "add key " + testRegistry.TrustKey()},
	{"trust configured, node behind", func(f *fakeAccess) { f.SSHReplies[certTest] = []resp{{Err: errors.New("exit 1")}} },
		"Registry trust", precheck.Warn, "doesn't have", "wait for the node's MachineConfig"},
	{"cluster degraded", func(f *fakeAccess) {
		f.OcReplies[cosGet] = []resp{{Out: `{"items":[{"metadata":{"name":"etcd"},"status":{"conditions":[{"type":"Available","status":"True"},{"type":"Degraded","status":"True"}]}}]}`}}
	}, "Cluster settled", precheck.Fail, "etcd degraded", "waits up to"},
	{"API down", func(f *fakeAccess) { f.OcReplies[cosGet] = []resp{{Err: errors.New("connection refused")}} },
		"vSNO API", precheck.Fail, "connection refused", "is the vSNO running?"},
}

func TestPrecheckChecks(t *testing.T) {
	for _, c := range checkCases {
		t.Run(c.name, func(t *testing.T) { checkRow(t, c) })
	}
}

func checkRow(t *testing.T, c checkCase) {
	t.Helper()
	fa := readyAccess()
	c.mutate(fa)
	_, rep := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
	got := rowOf(t, rep, c.check)
	if got.Status != c.want || !strings.Contains(got.Detail, c.detail) {
		t.Errorf("%s = %s %q, want %s with %q", c.check, got.Status, got.Detail, c.want, c.detail)
	}
	if c.hint != "" && !strings.Contains(got.Hint, c.hint) {
		t.Errorf("%s hint = %q, want %q", c.check, got.Hint, c.hint)
	}
}

func TestPrecheckRows(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		_, rep := inspect(t, readyAccess(), PrecheckOptions{Namespace: ns, Name: "a"})
		if n := rep.Failed(); n != 0 || len(rep.Results) != 7 {
			t.Fatalf("%d failed of %d: %+v", n, len(rep.Results), rep.Results)
		}
		for _, res := range rep.Results {
			if res.Status != precheck.Pass {
				t.Errorf("%s = %s %q", res.Check, res.Status, res.Detail)
			}
		}
		if rep.LCA != "lifecycle-agent.v4.20.7" {
			t.Errorf("LCA = %q", rep.LCA)
		}
	})

	t.Run("not ready", func(t *testing.T) {
		fa := readyAccess()
		in, rep := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
		in.g.State, in.g.Detail = guest.Stopped, ""
		in.rep.Results = nil
		in.run(context.Background())
		if got := rowOf(t, rep, "vSNO a"); got.Status != precheck.Fail || !strings.Contains(got.Detail, "stopped") || !strings.Contains(got.Hint, "vsno start a") {
			t.Errorf("vSNO row = %+v", got)
		}
	})

	t.Run("external image needs no trust", func(t *testing.T) {
		fa := readyAccess()
		in, rep := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
		in.Kube = registrySecrets(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-config", Name: "pull-secret"},
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(fakeAuth("quay.io", "u:p"))}})
		in.rep.Image = "quay.io/me/seed:1"
		in.rep.Results = nil
		in.run(context.Background())
		if got := rowOf(t, rep, "Registry trust"); got.Status != precheck.Info || !strings.Contains(got.Detail, "quay.io") {
			t.Errorf("trust row = %+v", got)
		}
	})

	t.Run("unknown version has no default image", func(t *testing.T) {
		fa := readyAccess()
		in, rep := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
		in.rep.Image = ""
		in.rep.Results = nil
		in.run(context.Background())
		if got := rowOf(t, rep, "Seed target"); got.Status != precheck.Fail || got.Hint != "pass --image <ref>" {
			t.Errorf("target row = %+v", got)
		}
	})

	t.Run("no push credentials", func(t *testing.T) {
		fa := readyAccess()
		in, rep := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
		in.Kube = kfake.NewClientset()
		in.rep.Image = "quay.io/me/seed:1"
		in.rep.Results = nil
		in.run(context.Background())
		if got := rowOf(t, rep, "Seed target"); got.Status != precheck.Fail || !strings.Contains(got.Hint, "pull secret") {
			t.Errorf("target row = %+v", got)
		}
	})
}

func TestPrecheckSettles(t *testing.T) {
	fa := readyAccess()
	fa.OcReplies[cosGet] = []resp{
		{Out: `{"items":[{"metadata":{"name":"etcd"},"status":{"conditions":[{"type":"Available","status":"True"},{"type":"Progressing","status":"True"}]}}]}`},
		{Out: cosStable},
	}
	_, rep := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a", Settle: true})
	if got := rowOf(t, rep, "Cluster settled"); got.Status != precheck.Pass {
		t.Errorf("settled = %+v", got)
	}
	if len(fa.Calls) < 3 {
		t.Errorf("calls %q", fa.Calls)
	}
}

func TestInstall(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[csvGet] = []resp{{Out: ""}, {Out: ""}, {Out: csvDone}}
	fa.OcReplies[pmGet] = []resp{{Out: `{"status":{"catalogSource":"redhat-operators","catalogSourceNamespace":"openshift-marketplace","defaultChannel":"stable"}}`}}
	fa.OcReplies[oadpCSVGet] = []resp{{Out: ""}, {Out: ""}, {Out: oadpDone}}
	fa.OcReplies[oadpPMGet] = []resp{{Out: `{"status":{"catalogSource":"redhat-operators","catalogSourceNamespace":"openshift-marketplace","defaultChannel":"stable"}}`}}
	fa.OcReplies[imgGet] = []resp{{Out: imgNone}, {Out: imgOurs}}
	fa.SSHReplies[certTest] = []resp{{Err: errors.New("exit 1")}, {}}
	in, rep := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
	if rep.Failed() == 0 {
		t.Fatal("a vSNO without LCA or trust passed")
	}

	install(ctx, in.Env, in)
	rep.Results = nil
	in.run(ctx)
	if n := rep.Failed(); n != 0 {
		t.Fatalf("%d failed after --install: %+v", n, rep.Results)
	}
	// LCA, OADP and trust: the same three vsno prepare sets up for a seed.
	want := []string{"Lifecycle Agent lifecycle-agent.v4.20.7", "OADP oadp-operator.v1.5.8", "the vSNO trusting " + testRegistry.Host}
	if len(rep.Warnings) != 0 || !slices.Equal(rep.Ready, want) {
		t.Errorf("ready %q, warnings %q; want %q", rep.Ready, rep.Warnings, want)
	}
	var subscribedLCA, subscribedOADP, trusted bool
	for _, s := range fa.Stdin {
		subscribedLCA = subscribedLCA || strings.Contains(s, `channel: "stable"`) && strings.Contains(s, "lifecycle-agent")
		subscribedOADP = subscribedOADP || strings.Contains(s, OADP.Package)
		trusted = trusted || strings.Contains(s, testRegistry.TrustKey())
	}
	if !subscribedLCA || !subscribedOADP || !trusted {
		t.Errorf("ready %q, applied %q", rep.Ready, fa.Stdin)
	}
}

// --install takes the same steps vsno prepare does, so a vSNO readied by one
// and by the other end up the same.
func TestInstallTakesPreparesSteps(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	in, _ := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
	p := &countingSteps{}
	in.Progress = p
	install(ctx, in.Env, in)
	want := []string{"the Lifecycle Agent", "OADP", "trusting the environment registry"}
	if !slices.Equal(p.titles, want) {
		t.Errorf("steps %q, want %q", p.titles, want)
	}
}

// A step nobody finishes keeps redrawing over the report that follows it: every
// step install starts has to end.
func TestInstallFinishesItsSteps(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[csvGet] = []resp{{Out: ""}, {Out: ""}, {Out: csvDone}}
	fa.OcReplies[pmGet] = []resp{{Out: `{"status":{"catalogSource":"redhat-operators","catalogSourceNamespace":"openshift-marketplace","defaultChannel":"stable"}}`}}
	fa.OcReplies[oadpCSVGet] = []resp{{Out: ""}, {Out: ""}, {Out: oadpDone}}
	fa.OcReplies[oadpPMGet] = []resp{{Out: `{"status":{"catalogSource":"redhat-operators","catalogSourceNamespace":"openshift-marketplace","defaultChannel":"stable"}}`}}
	fa.OcReplies[imgGet] = []resp{{Out: imgNone}, {Out: imgOurs}}
	fa.SSHReplies[certTest] = []resp{{Err: errors.New("exit 1")}, {}}
	in, _ := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
	p := &countingSteps{}
	in.Progress = p
	install(ctx, in.Env, in)
	if p.started == 0 || p.started != p.finished {
		t.Errorf("started %d steps, finished %d", p.started, p.finished)
	}
}

// countingSteps records the steps a run starts and ends.
type countingSteps struct {
	started, finished int
	titles            []string
}

type countingStep countingSteps

func (c *countingSteps) Start(title string) precheck.ProgressStep {
	c.started++
	c.titles = append(c.titles, title)
	return (*countingStep)(c)
}
func (s *countingStep) Update(string)        {}
func (s *countingStep) SetProgress(int, int) {}
func (s *countingStep) Done(string)          { s.finished++ }
func (s *countingStep) Warn(string)          { s.finished++ }
func (s *countingStep) Fail(string)          { s.finished++ }

func TestInstallLeavesOthersAlone(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[imgGet] = []resp{{Out: imgElse}}
	in, rep := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
	install(ctx, in.Env, in)
	// Both operators are already there and the CA ConfigMap is someone else's,
	// so --install set up nothing and leaves the ConfigMap alone.
	if len(rep.Ready) != 0 || fa.Ran("oc patch image.config.openshift.io") || len(fa.Stdin) != 0 {
		t.Errorf("ready %q, calls %q", rep.Ready, fa.Calls)
	}
	if got := rowOf(t, rep, "Registry trust"); !strings.Contains(got.Hint, "openshift-config/corp-ca") {
		t.Errorf("hint = %q", got.Hint)
	}
}

func TestInstallWarnsInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	fa := readyAccess()
	fa.OcReplies[csvGet] = []resp{{Out: ""}}
	fa.OcReplies[pmGet] = []resp{{Out: ""}} // no package: LCA can't be installed
	in, rep := inspect(t, fa, PrecheckOptions{Namespace: ns, Name: "a"})
	install(ctx, in.Env, in)
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "no lifecycle-agent package") {
		t.Errorf("warnings %q", rep.Warnings)
	}
}

// probeRunner answers probe.sh, so guest.List sees a ready vSNO.
type probeRunner struct {
	out string
	err error
}

func (p probeRunner) Run(_ context.Context, _ string, _ []string, streams builder.IO) error {
	_, _ = io.WriteString(streams.Stdout, p.out)
	return p.err
}

var vmListKinds = map[schema.GroupVersionResource]string{cluster.GVRVirtualMachine: "VirtualMachineList"}

// world is an environment with vSNO a in it.
func world(t *testing.T, fa *fakeAccess) Env {
	t.Helper()
	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"namespace": ns, "name": "a", "annotations": map[string]any{guest.AnnVersion: "4.20.46"}},
		"status":   map[string]any{"printableStatus": "Running"},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), vmListKinds, vm)
	k := registrySecrets()
	if _, err := k.CoreV1().Services(ns).Create(context.Background(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "a"}, Spec: corev1.ServiceSpec{ClusterIP: "172.30.1.9"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e := testEnv(fa)
	e.Kube, e.Dyn = k, dyn
	e.Builder = probeRunner{out: `{"name":"a","cv":{"version":"4.20.46","available":true}}`}
	e.Poll = time.Millisecond
	return e
}

func TestPrecheck(t *testing.T) {
	ctx := context.Background()
	t.Run("ready", func(t *testing.T) {
		rep, err := Precheck(ctx, world(t, readyAccess()), PrecheckOptions{Namespace: ns, Name: "a"})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Failed() != 0 || rep.Version != "4.20.46" || rep.Image != Ref(testRegistry, "a", "4.20.46") {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("missing vSNO", func(t *testing.T) {
		fa := readyAccess()
		e := world(t, fa)
		e.Dyn = dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), vmListKinds)
		rep, err := Precheck(ctx, e, PrecheckOptions{Namespace: ns, Name: "ghost"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Results) != 1 || rep.Failed() != 1 || !strings.Contains(rep.Results[0].Detail, "not found") {
			t.Errorf("report = %+v", rep)
		}
		if len(fa.Calls) != 0 {
			t.Errorf("checked a vSNO that isn't there: %q", fa.Calls)
		}
	})
}

// TestPrecheckErrors covers the ways Precheck itself fails, rather than a
// vSNO being unready.
func TestPrecheckErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("listing fails", func(t *testing.T) {
		e := world(t, readyAccess())
		dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), vmListKinds)
		dyn.PrependReactor("list", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("api down")
		})
		e.Dyn = dyn
		if _, err := Precheck(ctx, e, PrecheckOptions{Namespace: ns, Name: "a"}); err == nil || !strings.Contains(err.Error(), "api down") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing registry", func(t *testing.T) {
		e := world(t, readyAccess())
		e.Kube = kfake.NewClientset()
		rep, err := Precheck(ctx, e, PrecheckOptions{Namespace: ns, Name: "a"})
		if err != nil {
			t.Fatal(err)
		}
		if got := rowOf(t, rep, "Registry"); got.Status != precheck.Fail || !strings.Contains(got.Detail, "registry-auth") {
			t.Errorf("registry row = %+v", got)
		}
		for _, res := range rep.Results {
			if res.Check == "Registry trust" {
				t.Errorf("trust checked without a registry: %+v", res)
			}
		}
	})
}

// The registry has to hold the seed image this run pushes; say so before the
// vSNO's services are stopped, not after.
func TestPrecheckRegistryRoom(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		total  int64
		used   int64
		status precheck.Status
		detail string
	}{
		{"plenty", 100 << 30, 40 << 30, precheck.Pass, "60.0Gi free of 100.0Gi"},
		{"tight", 100 << 30, 97 << 30, precheck.Warn, "3.0Gi free of 100.0Gi"},
		{"full", 100 << 30, 99 << 30, precheck.Fail, "1.0Gi free of 100.0Gi"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := world(t, readyAccess())
			e.Volume = func(context.Context, string) (int64, int64, error) { return c.used, c.total, nil }
			rep, err := Precheck(ctx, e, PrecheckOptions{Namespace: ns, Name: "a"})
			if err != nil {
				t.Fatal(err)
			}
			res := rowOf(t, rep, checkRoom)
			if res.Status != c.status || !strings.Contains(res.Detail, c.detail) {
				t.Errorf("row = %+v, want %s %q", res, c.status, c.detail)
			}
		})
	}

	// A volume that can't be read is a warning, not a failed precheck.
	e := world(t, readyAccess())
	e.Volume = func(context.Context, string) (int64, int64, error) { return 0, 0, errors.New("no registry pod") }
	rep, err := Precheck(ctx, e, PrecheckOptions{Namespace: ns, Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if res := rowOf(t, rep, checkRoom); res.Status != precheck.Warn || !strings.Contains(res.Detail, "couldn't be read") {
		t.Errorf("unreadable volume: %+v", res)
	}
	if rep.Failed() != 0 {
		t.Errorf("failed checks: %d", rep.Failed())
	}

	// A seed going to another registry does not consult this one's volume.
	e = world(t, readyAccess())
	called := false
	e.Volume = func(context.Context, string) (int64, int64, error) { called = true; return 1, 1, nil }
	if _, err := Precheck(ctx, e, PrecheckOptions{Namespace: ns, Name: "a", Image: "quay.io/someone/seed:1.2.3"}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("it read the volume for an image elsewhere")
	}
}
