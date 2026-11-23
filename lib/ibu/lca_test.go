package ibu

import (
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
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
	k8stesting "k8s.io/client-go/testing"
)

const (
	lcaDeployGet = "get deployments lifecycle-agent-controller-manager -n openshift-lifecycle-agent"
	lcaImage     = "registry.vsno-test.svc.cluster.local:5000/lca:0123456789ab"
)

var testReg = seed.Registry{Host: "registry.vsno-test.svc.cluster.local:5000", User: "u", Password: "p", CA: "CA"}

func TestLCASource(t *testing.T) {
	if !(LCASource{}).FromCatalog() || (LCASource{Ref: "main"}).FromCatalog() || (LCASource{Image: "x"}).FromCatalog() {
		t.Error("FromCatalog")
	}
	if s := (LCASource{Image: "x"}).withDefaults(); s.Repo != DefaultLCARepo || s.Ref != "main" {
		t.Errorf("defaults = %+v", s)
	}
	if s := (LCASource{Repo: "https://github.com/me/lca", Ref: "fix"}).withDefaults(); s.Repo != "https://github.com/me/lca" || s.Ref != "fix" {
		t.Errorf("explicit = %+v", s)
	}
}

func TestParseBuild(t *testing.T) {
	b, err := parseBuild("cloning x at main\nsha=0123456789ab\nversion=4.21.0\nbuilding x\nimage=" + lcaImage + "\nbuilt\n")
	if err != nil || b.SHA != "0123456789ab" || b.Version != "4.21.0" || b.Image != lcaImage {
		t.Fatalf("= %+v, %v", b, err)
	}
	b.Repo = DefaultLCARepo
	if got := b.String(); got != DefaultLCARepo+"@0123456789ab ("+lcaImage+")" {
		t.Errorf("String = %q", got)
	}
	if _, err := parseBuild("cloning x\nfatal: couldn't find remote ref nope\n"); err == nil || !strings.Contains(err.Error(), "couldn't find remote ref nope") {
		t.Errorf("err = %v", err)
	}
}

// deployment is LCA's controller Deployment running image, with one replica
// updated and available ones of it ready.
func deployment(image string, available int) guestctltest.Reply {
	return rollout(image, 1, 1, available, 1-available)
}

// rollout is LCA's controller Deployment with the given replica counts.
func rollout(image string, replicas, updated, available, unavailable int) guestctltest.Reply {
	return guestctltest.Reply{Out: `{"spec":{"template":{"spec":{"containers":[{"name":"manager","image":"` + image + `"}]}}},` +
		`"status":{"replicas":` + strconv.Itoa(replicas) + `,"updatedReplicas":` + strconv.Itoa(updated) +
		`,"availableReplicas":` + strconv.Itoa(available) + `,"unavailableReplicas":` + strconv.Itoa(unavailable) + `}}`}
}

func TestInstalledLCA(t *testing.T) {
	ctx := context.Background()
	if name, err := seed.InstalledLCA(ctx, targetFake()); err != nil || name != "lifecycle-agent.v4.21.5" {
		t.Errorf("catalog: %q %v", name, err)
	}
	f := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{lcaDeployGet + " --ignore-not-found": {deployment(lcaImage, 1)}}}
	if name, err := seed.InstalledLCA(ctx, f); err != nil || name != lcaImage {
		t.Errorf("from source: %q %v", name, err)
	}
	// OLM's own Deployment of the same name isn't "from source".
	olm := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{lcaDeployGet + " --ignore-not-found": {{Out: `{"metadata":{"labels":{"olm.owner":"lifecycle-agent.v4.21.5"}}}`}}}}
	if _, fromSource, err := seed.SourceLCA(ctx, olm); err != nil || fromSource {
		t.Errorf("OLM's deployment: fromSource = %v %v", fromSource, err)
	}
	// OLM's Deployment answers with the image it runs, so an operator installed
	// by the catalog and one deployed from that image compare equal.
	if name, err := seed.InstalledLCA(ctx, olm); err != nil || name != "" {
		t.Errorf("OLM's deployment without an image: %q %v", name, err)
	}
	olmImage := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{
		lcaDeployGet + " --ignore-not-found": {deployment(lcaImage, 1)}}}
	if name, err := seed.InstalledLCA(ctx, olmImage); err != nil || name != lcaImage {
		t.Errorf("OLM's deployment with an image: %q %v", name, err)
	}
	if name, err := seed.InstalledLCA(ctx, &guestctltest.Fake{}); err != nil || name != "" {
		t.Errorf("none: %q %v", name, err)
	}
}

func pullSecretReply(cfg string) guestctltest.Reply {
	return guestctltest.Reply{Out: `{"data":{".dockerconfigjson":"` + base64.StdEncoding.EncodeToString([]byte(cfg)) + `"}}`}
}

func TestDeployLCA(t *testing.T) {
	ctx := context.Background()
	e := testEnv(nil)
	b := &BuiltLCA{SHA: "0123456789ab", Image: lcaImage, manifests: "kind: Deployment\n"}
	f := targetFake() // runs the catalog's LCA
	f.OcReplies["get secret pull-secret -n openshift-config"] = []guestctltest.Reply{pullSecretReply(`{"auths":{"quay.io":{"auth":"QQ=="}}}`)}
	// Mid-rollout the old controller is still available and the new one not: not yet.
	f.OcReplies[lcaDeployGet] = []guestctltest.Reply{deployment("old", 1), rollout(lcaImage, 2, 1, 1, 1), deployment(lcaImage, 0), deployment(lcaImage, 1)}
	if err := deployLCA(ctx, f, e.Clock(), b, testReg, &steps{}); err != nil {
		t.Fatal(err)
	}
	if n := slices.Index(f.Calls, "oc "+lcaDeployGet); n < 0 || len(f.Calls)-n != 4 {
		t.Errorf("polled the Deployment %d time(s) from %q; want 4: done only once fully rolled out", len(f.Calls)-n, f.Calls)
	}
	for _, want := range []string{
		"oc delete subscriptions lifecycle-agent -n openshift-lifecycle-agent --ignore-not-found",
		"oc delete clusterserviceversions lifecycle-agent.v4.21.5 -n openshift-lifecycle-agent --ignore-not-found",
		"oc patch secret pull-secret -n openshift-config --type merge",
		"oc apply -f -",
	} {
		if !f.Ran(want) {
			t.Errorf("didn't run %q: %q", want, f.Calls)
		}
	}

	// A guest that already pulls from the registry, as one installed from the
	// cache does, keeps its pull secret; a controller that never runs b times out.
	g := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{
		"get secret pull-secret -n openshift-config": {pullSecretReply(`{"auths":{"` + testReg.Host + `":{"auth":"dTpw"}}}`)},
		lcaDeployGet: {deployment("old", 1)},
	}}
	err := deployLCA(ctx, g, e.Clock(), b, testReg, &steps{})
	if g.Ran("oc patch secret") || err == nil || !strings.Contains(err.Error(), "didn't start in") {
		t.Errorf("err = %v, calls = %q", err, g.Calls)
	}
}

func TestVersionWarning(t *testing.T) {
	ctx := context.Background()
	b := &BuiltLCA{Ref: "main", SHA: "abc", Version: "5.1.0"}
	if w := versionWarning(ctx, targetFake(), b); len(w) != 1 || !strings.Contains(w[0], "is version 5.1.0, but the vSNO runs OpenShift 4.21.1") {
		t.Errorf("warnings = %q", w)
	}
	b.Version = "4.21.0"
	if w := versionWarning(ctx, targetFake(), b); len(w) != 0 {
		t.Errorf("same minor warned: %q", w)
	}
}

// A vSNO already running LCA from a ref keeps it on a catalog prepare, such as
// the one vsno upgrade runs: subscribing would replace it.
func TestPrepareKeepsLCAFromSource(t *testing.T) {
	f := targetFake()
	f.OcReplies[lcaCSVs] = []guestctltest.Reply{{Out: `{"items":[]}`}}
	f.OcReplies[lcaDeployGet+" --ignore-not-found"] = []guestctltest.Reply{deployment(lcaImage, 1)}
	out, err := Prepare(context.Background(), testEnv(map[string]guestctl.Access{"t": f}), PrepareOptions{Namespace: ns, Names: []string{"t"}, Role: RoleSeed})
	if err != nil || out[0].LCA != lcaImage || f.Ran("oc get packagemanifests lifecycle-agent") {
		t.Errorf("out = %+v, err = %v, calls = %q", out, err, f.Calls)
	}
}

// The build pod runs on the builder's node; its outcome comes from its phase
// and its log. The fake clientset's log is a fixed string, so a pod that
// succeeds without reporting its commit is the "didn't report" error.
func TestRunBuildPod(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		phase corev1.PodPhase
		want  string
	}{
		{corev1.PodFailed, "the LCA build failed"},
		{corev1.PodSucceeded, "didn't report its commit"},
	} {
		t.Run(string(c.phase), func(t *testing.T) {
			e := testEnv(nil)
			k := e.Kube.(*kfake.Clientset)
			var created *corev1.Pod
			k.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				created = a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
				return false, nil, nil
			})
			k.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: lcaBuildPod, Namespace: ns}, Status: corev1.PodStatus{Phase: c.phase}}, nil
			})
			logs, err := runBuildPod(ctx, e, ns, testBuilder(), []string{DefaultLCARepo, "main", testReg.Host, ""}, &steps{})
			if err == nil {
				_, err = parseBuild(logs)
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
			if created == nil || created.Spec.NodeName != "worker-1" || !*created.Spec.Containers[0].SecurityContext.Privileged ||
				created.Spec.SecurityContext == nil || created.Spec.SecurityContext.SELinuxOptions.Level != "s0:c29,c4" {
				t.Errorf("pod = %+v", created)
			}
		})
	}
}

// testBuilder is a running builder pod on worker-1 at SELinux level s0:c29,c4.
func testBuilder() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "builder-x", Namespace: ns, Labels: map[string]string{"app": "builder"}},
		Spec:   corev1.PodSpec{NodeName: "worker-1", SecurityContext: &corev1.PodSecurityContext{SELinuxOptions: &corev1.SELinuxOptions{Level: "s0:c29,c4"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

// Without SELinux options on the builder, the build pod sets none either.
func TestSameSELinux(t *testing.T) {
	if sc := sameSELinux(&corev1.Pod{}); sc != nil {
		t.Errorf("= %+v", sc)
	}
	b := testBuilder()
	sc := sameSELinux(b)
	b.Spec.SecurityContext.SELinuxOptions.Level = "changed"
	if sc == nil || sc.SELinuxOptions.Level != "s0:c29,c4" {
		t.Errorf("= %+v", sc)
	}
}

func TestWriteBuildAuth(t *testing.T) {
	ctx := context.Background()
	e := testEnv(nil)
	if err := writeBuildAuth(ctx, e, ns, testReg); err == nil || !strings.Contains(err.Error(), "pull secret") {
		t.Errorf("no cluster pull secret: err = %v", err)
	}
	_, _ = e.Kube.CoreV1().Secrets("openshift-config").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: "openshift-config"},
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"registry.redhat.io":{"auth":"QQ=="}}}`)}}, metav1.CreateOptions{})
	for range 2 { // a second write replaces the first
		if err := writeBuildAuth(ctx, e, ns, testReg); err != nil {
			t.Fatal(err)
		}
	}
	s, err := e.Kube.CoreV1().Secrets(ns).Get(ctx, lcaBuildAuth, metav1.GetOptions{})
	if err != nil || !strings.Contains(s.StringData["auth.json"], "registry.redhat.io") || !strings.Contains(s.StringData["auth.json"], testReg.Host) || s.StringData["ca.crt"] != "CA" {
		t.Errorf("secret = %+v, err = %v", s.StringData, err)
	}
}

func TestRecordLCA(t *testing.T) {
	ctx := context.Background()
	e := testEnv(nil)
	vm := &unstructured.Unstructured{Object: map[string]any{}}
	vm.SetAPIVersion("kubevirt.io/v1")
	vm.SetKind("VirtualMachine")
	vm.SetNamespace(ns)
	vm.SetName("t")
	e.Dyn = dynfake.NewSimpleDynamicClient(runtime.NewScheme(), vm)
	b := &BuiltLCA{Repo: DefaultLCARepo, SHA: "abc", Image: lcaImage}
	if err := recordLCA(ctx, e, ns, "t", b); err != nil {
		t.Fatal(err)
	}
	got, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, "t", metav1.GetOptions{})
	if err != nil || got.GetAnnotations()[guest.AnnLCASource] != b.String() {
		t.Errorf("annotations = %v, err = %v", got.GetAnnotations(), err)
	}
	if err := recordLCA(ctx, e, ns, "not-a-vsno", b); err != nil {
		t.Errorf("a VM vsno didn't make: err = %v", err)
	}
}

func TestLCAStepName(t *testing.T) {
	if got := (LCASource{Ref: "main"}).stepName(); got != "Building LCA" {
		t.Errorf("ref: %q", got)
	}
	if got := (LCASource{Image: "quay.io/x/lca:1"}).stepName(); got != "Fetching LCA's manifests" {
		t.Errorf("image: %q", got)
	}
}

// A vSNO installed from the release cache already has the registry's login:
// its pull secret is left alone. One without it gets it merged in.
func TestEnsurePullAuth(t *testing.T) {
	ctx := context.Background()
	get := "get secret pull-secret -n openshift-config"
	have := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{
		get: {pullSecretReply(`{"auths":{"` + testReg.Host + `":{"auth":"dTpw"}}}`)},
	}}
	if err := ensurePullAuth(ctx, have, testReg); err != nil || have.Ran("oc patch secret pull-secret") {
		t.Errorf("already there: %v, calls %q", err, have.Calls)
	}
	lacking := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{
		get: {pullSecretReply(`{"auths":{"quay.io":{"auth":"QQ=="}}}`)},
	}}
	if err := ensurePullAuth(ctx, lacking, testReg); err != nil || !lacking.Ran("oc patch secret pull-secret -n openshift-config --type merge") {
		t.Errorf("missing: %v, calls %q", err, lacking.Calls)
	}
}
