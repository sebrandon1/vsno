package guest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/release"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func createOpts(name, version string) CreateOptions {
	return CreateOptions{Namespace: ns, Name: name, Version: version, Cores: 16, MemoryGiB: 32, DiskGiB: 200}
}

func TestCreateNoWait(t *testing.T) {
	w := newWorld(t)
	g, err := Create(context.Background(), w.env(), createOpts("seed", "4.20"))
	if err != nil {
		t.Fatal(err)
	}
	if g.Version != "4.20.46" || g.Node != node || g.API != "https://api.seed.vsno.lab:6443" {
		t.Errorf("guest = %+v", g)
	}
	for _, gvr := range []string{"Service", "DataVolume", "VirtualMachine"} {
		if _, err := w.dyn.Resource(gvrs[gvr]).Namespace(ns).Get(context.Background(), map[string]string{"Service": "seed", "DataVolume": "seed-iso", "VirtualMachine": "seed"}[gvr], metav1.GetOptions{}); err != nil {
			t.Errorf("%s: %v", gvr, err)
		}
	}
	if w.dnsKeys(t)["seed.conf"] == "" {
		t.Error("no DNS entry")
	}
	iso := w.runner.ran("iso.sh")
	if len(iso) != 1 || iso[0].args[0] != "seed" || iso[0].args[1] != "quay.io/openshift-release-dev/ocp-release:4.20.46-x86_64" {
		t.Fatalf("iso.sh calls = %+v", iso)
	}
	// The tarball holds the installer's inputs, the pull secret, and dnsmasq as the guest's resolver.
	for _, want := range []string{"config/install-config.yaml", "config/agent-config.yaml", "config/openshift/98-var-lib-containers-partitioned.yaml", "pull-secret.json", "172.30.0.53"} {
		if !strings.Contains(iso[0].stdin, want) {
			t.Errorf("ISO tarball lacks %q", want)
		}
	}
}

func TestCreatePullspecRecordsVersion(t *testing.T) {
	w := newWorld(t)
	w.runner.outputs["iso.sh"] = "version 4.21.3\nISO ready\n"
	g, err := Create(context.Background(), w.env(), createOpts("x", "quay.io/example/release@sha256:abc"))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "x", metav1.GetOptions{})
	if g.Version != "4.21.3" || u.GetAnnotations()[AnnVersion] != "4.21.3" {
		t.Errorf("version: guest %q, annotation %q", g.Version, u.GetAnnotations()[AnnVersion])
	}
}

func TestCreateRefusals(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*world, *Env, *CreateOptions)
		want  string
	}{
		{"bad name", func(_ *world, _ *Env, o *CreateOptions) { o.Name = "Bad" }, "invalid name"},
		{"too small", func(_ *world, _ *Env, o *CreateOptions) { o.MemoryGiB = 8 }, "--memory 8"},
		{"no namespace", func(_ *world, _ *Env, o *CreateOptions) { o.Namespace = "elsewhere" }, "run `vsno setup -n elsewhere` first"},
		{"not vsno's", func(w *world, _ *Env, _ *CreateOptions) {
			n, _ := w.kube.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
			n.Labels = nil
			_, _ = w.kube.CoreV1().Namespaces().Update(context.Background(), n, metav1.UpdateOptions{})
			_ = w.kube.CoreV1().ConfigMaps(ns).Delete(context.Background(), "vsno-state", metav1.DeleteOptions{})
		}, "was not created by vsno setup"},
		{"no builder", func(w *world, _ *Env, _ *CreateOptions) {
			_ = w.kube.CoreV1().Pods(ns).Delete(context.Background(), "builder-1", metav1.DeleteOptions{})
		}, "no ready builder pod"},
		{"exists", func(w *world, _ *Env, _ *CreateOptions) {
			_ = w.dyn.Tracker().Create(cluster.GVRVirtualMachine, vm("seed", "Running"), ns)
		}, "vSNO seed already exists"},
		{"no node", func(w *world, _ *Env, _ *CreateOptions) {
			// The namespace is still labeled, so it counts as vsno's without the state.
			_ = w.kube.CoreV1().ConfigMaps(ns).Delete(context.Background(), "vsno-state", metav1.DeleteOptions{})
		}, "no node recorded"},
		{"too many cores", func(_ *world, _ *Env, o *CreateOptions) { o.Cores = 200 }, "--cores 200 is more than node"},
		{"no memory", func(w *world, _ *Env, _ *CreateOptions) {
			_, _ = w.kube.CoreV1().Pods("other").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: "other"},
				Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("70Gi")}}}}}}, metav1.CreateOptions{})
		}, "has 20 GiB of memory free; a 32 GiB vSNO needs 33 GiB"},
		{"bad version", func(_ *world, e *Env, _ *CreateOptions) { e.Resolver = fakeResolver{err: errBoom} }, "boom"},
		{"nightly without CI credentials", func(w *world, _ *Env, o *CreateOptions) {
			o.Version = "4.22-nightly"
			s, _ := w.kube.CoreV1().Secrets("openshift-config").Get(context.Background(), "pull-secret", metav1.GetOptions{})
			s.Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{"quay.io":{"auth":"x"}}}`)
			_, _ = w.kube.CoreV1().Secrets("openshift-config").Update(context.Background(), s, metav1.UpdateOptions{})
		}, "needs credentials for registry.ci.openshift.org"},
		{"no ssh key", func(w *world, _ *Env, _ *CreateOptions) {
			_ = w.kube.CoreV1().Secrets(ns).Delete(context.Background(), "vsno-ssh", metav1.DeleteOptions{})
		}, "vsno-ssh is missing; rerun `vsno setup -n vsno-test`"},
		{"no dnsmasq", func(w *world, _ *Env, _ *CreateOptions) {
			_ = w.kube.CoreV1().Services(ns).Delete(context.Background(), "dnsmasq", metav1.DeleteOptions{})
		}, "reading the dnsmasq Service"},
		{"no pull secret", func(w *world, _ *Env, _ *CreateOptions) {
			_ = w.kube.CoreV1().Secrets("openshift-config").Delete(context.Background(), "pull-secret", metav1.DeleteOptions{})
		}, "reading the cluster pull secret"},
		{"bad pull secret", func(w *world, _ *Env, _ *CreateOptions) {
			s, _ := w.kube.CoreV1().Secrets("openshift-config").Get(context.Background(), "pull-secret", metav1.GetOptions{})
			s.Data[corev1.DockerConfigJsonKey] = []byte("not json")
			_, _ = w.kube.CoreV1().Secrets("openshift-config").Update(context.Background(), s, metav1.UpdateOptions{})
		}, "parsing the cluster pull secret"},
		{"ssh key unreadable", func(w *world, _ *Env, _ *CreateOptions) {
			w.kube.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.(k8stesting.GetAction).GetName() == "vsno-ssh" {
					return true, nil, errBoom
				}
				return false, nil, nil
			})
		}, "reading secret vsno-ssh: boom"},
		{"node unreadable", func(w *world, _ *Env, _ *CreateOptions) {
			w.kube.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errBoom })
		}, "reading node worker-1"},
		{"ISO build fails", func(w *world, _ *Env, _ *CreateOptions) { w.runner.errs["iso.sh"] = errBoom }, "building the ISO: boom: last line"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			e, o := w.env(), createOpts("seed", "4.20")
			c.setup(w, &e, &o)
			_, err := Create(context.Background(), e, o)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if c.name != "ISO build fails" {
				if _, err := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "seed", metav1.GetOptions{}); err == nil && c.name != "exists" {
					t.Error("a refused create made a VM")
				}
			}
		})
	}
}

func TestSetDNS(t *testing.T) {
	w := newWorld(t)
	e := w.env()
	ctx := context.Background()
	if err := SetDNS(ctx, e, ns, "a", false); err != nil || w.dnsKeys(t) != nil {
		t.Fatalf("removing from no ConfigMap: %v %v", err, w.dnsKeys(t))
	}
	for _, n := range []string{"a", "b"} {
		if err := SetDNS(ctx, e, ns, n, true); err != nil {
			t.Fatal(err)
		}
	}
	if k := w.dnsKeys(t); len(k) != 2 || !strings.Contains(k["b.conf"], "address=/api.b.vsno.lab/10.0.2.2") {
		t.Fatalf("keys = %v", k)
	}
	for range 2 { // removing twice is fine
		if err := SetDNS(ctx, e, ns, "a", false); err != nil {
			t.Fatal(err)
		}
	}
	if k := w.dnsKeys(t); len(k) != 1 || k["b.conf"] == "" {
		t.Errorf("keys = %v", k)
	}
	w.kube.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errBoom })
	if err := SetDNS(ctx, e, ns, "c", true); err == nil {
		t.Error("want the read error")
	}
}

// installing makes the vSNO's VM, DataVolume and Service look like an install
// that the builder probes with lines.
func installing(t *testing.T, w *world, lines ...string) Env {
	t.Helper()
	ctx := context.Background()
	for _, o := range []runtime.Object{vm("seed", "Running"), dv("seed-iso", "Succeeded", "100.0%")} {
		gvr := cluster.GVRVirtualMachine
		if strings.HasSuffix(o.(interface{ GetName() string }).GetName(), "-iso") {
			gvr = GVRDataVolume
		}
		if err := w.dyn.Tracker().Create(gvr, o, ns); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.kube.CoreV1().Services(ns).Create(ctx, svc("seed", "172.30.9.9"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	w.runner.outputs["probe.sh"] = strings.Join(lines, "")
	return w.env()
}

func TestWaitInstalled(t *testing.T) {
	ctx := context.Background()
	ready := probeLine("seed", `"cv":{"version":"4.20.46","available":true},"unhealthy":0`)
	w := newWorld(t)
	g := &Guest{Name: "seed"}
	if err := waitInstalled(ctx, installing(t, w, ready), g, createOpts("seed", "4.20"), time.Now()); err != nil || g.State != Ready || g.Version != "4.20.46" {
		t.Fatalf("ready: %v %+v", err, g)
	}
	if p := w.runner.ran("probe.sh"); len(p) == 0 || strings.Join(p[0].args, " ") != "vsno.lab seed=172.30.9.9" {
		t.Errorf("probe args = %+v", p)
	}

	failed := probeLine("seed", `"agent":{"status":"error","info":"disk too small"}`)
	g = &Guest{Name: "seed"}
	if err := waitInstalled(ctx, installing(t, newWorld(t), failed), g, createOpts("seed", "4.20"), time.Now()); err == nil || !strings.Contains(err.Error(), "install error: disk too small") {
		t.Errorf("failed: %v", err)
	}

	stuck := probeLine("seed", `"agent":{"status":"insufficient","validations":"{\"network\":[{\"id\":\"dns-wildcard-not-configured\",\"status\":\"failure\"}]}"}`)
	o := createOpts("seed", "4.20")
	o.StuckAfter = time.Millisecond
	if err := waitInstalled(ctx, installing(t, newWorld(t), stuck), &Guest{Name: "seed"}, o, time.Now()); err == nil || !strings.Contains(err.Error(), "dns-wildcard-not-configured") {
		t.Errorf("stuck: %v", err)
	}

	slow := probeLine("seed", `"agent":{"status":"installing","progress":10}`)
	e := installing(t, newWorld(t), slow)
	e.Timeout = 5 * time.Millisecond
	if err := waitInstalled(ctx, e, &Guest{Name: "seed"}, createOpts("seed", "4.20"), time.Now()); err == nil || !strings.Contains(err.Error(), "not ready after") {
		t.Errorf("timeout: %v", err)
	}

	gone := newWorld(t).env()
	if err := waitInstalled(ctx, gone, &Guest{Name: "seed"}, createOpts("seed", "4.20"), time.Now()); err == nil || !strings.Contains(err.Error(), "reading vSNO seed") {
		t.Errorf("missing VM: %v", err)
	}
}

func TestRecord(t *testing.T) {
	w := newWorld(t)
	if err := record(context.Background(), w.env(), ns, &Guest{Name: "seed"}); err != nil {
		t.Errorf("nothing to record: %v", err)
	}
	if err := record(context.Background(), w.env(), ns, &Guest{Name: "seed", Version: "4.20.46"}); err == nil {
		t.Error("want an error for a missing VM")
	}
	u, err := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "a", metav1.GetOptions{})
	if err != nil {
		t.Skip("the world's VM is named a") // the fakes' VM name
	}
	_ = u
}

func TestDur(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{0, ""}, {-time.Second, ""}, {time.Second, "1s"}, {8*time.Minute + 12*time.Second, "8m12s"}, {500 * time.Millisecond, "1s"}} {
		if got := dur(c.d); got != c.want {
			t.Errorf("dur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestCreateWaits(t *testing.T) {
	w := newWorld(t)
	w.runner.outputs["probe.sh"] = probeLine("seed", `"cv":{"version":"4.20.46","available":true}`)
	o := createOpts("seed", "4.20")
	o.Wait = true
	e := w.env()
	// The fake applier creates the VM without a status; mark it running as the wait starts.
	e.Applier = statusApplier{tracker{w.dyn}}
	if _, err := w.kube.CoreV1().Services(ns).Create(context.Background(), svc("seed", "172.30.9.9"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	g, err := Create(context.Background(), e, o)
	if err != nil || g.State != Ready {
		t.Fatalf("%v %+v", err, g)
	}
	u, err := w.dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(context.Background(), "seed", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ann := u.GetAnnotations()
	if ann[AnnVersion] != "4.20.46" || ann[AnnTimingISO] == "" || ann[AnnTimingInstall] == "" || ann[AnnTimingAvailable] == "" {
		t.Errorf("annotations = %v", ann)
	}
	// No Finishing was observed, so the reboot phase has nothing to report.
	if _, ok := ann[AnnTimingReboot]; ok {
		t.Errorf("reboot annotation without a reboot: %v", ann)
	}
	if _, ok := ann[AnnCached]; ok {
		t.Errorf("cached annotation without a cache: %v", ann)
	}
}

// A watch outlives a blip: one failed read of the vSNO must not kill a 45
// minute install.
func TestCreateWaitsThroughATransientError(t *testing.T) {
	w := newWorld(t)
	w.runner.outputs["probe.sh"] = probeLine("seed", `"cv":{"version":"4.20.46","available":true}`)
	o := createOpts("seed", "4.20")
	o.Wait = true
	e := w.env()
	e.Applier = statusApplier{tracker{w.dyn}}
	if _, err := w.kube.CoreV1().Services(ns).Create(context.Background(), svc("seed", "172.30.9.9"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	fails := 1
	w.dyn.PrependReactor("get", "virtualmachines", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.GetAction).GetName()
		// Let prepare's "does it exist" check through; fail only reads of the
		// VM that create just made, which is what the watch does.
		if _, err := w.dyn.Tracker().Get(cluster.GVRVirtualMachine, ns, name); err != nil || fails == 0 {
			return false, nil, nil
		}
		fails--
		return true, nil, errors.New("the API server is restarting")
	})
	g, err := Create(context.Background(), e, o)
	if err != nil || g.State != Ready {
		t.Fatalf("%v %+v", err, g)
	}
	if fails != 0 {
		t.Errorf("the reactor still fails: %d", fails)
	}
}

// statusApplier applies, then gives VMs a Running status like KubeVirt would.
type statusApplier struct{ tracker }

func (s statusApplier) Apply(ctx context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	for _, o := range objs {
		if o.GetKind() == "VirtualMachine" {
			o.Object["status"] = map[string]any{"printableStatus": "Running"}
		}
		if o.GetKind() == "Service" {
			continue // the test created it with a ClusterIP
		}
		if _, err := s.tracker.Apply(ctx, []*unstructured.Unstructured{o}); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func TestPercent(t *testing.T) {
	for _, c := range []struct {
		st     State
		detail string
		want   int
	}{
		{Importing, "45.10%", 45}, {Installing, "installing-in-progress 72%", 72}, {Installing, "waiting on validations: x", 0},
		{Finishing, "Working towards 4.20.46: 80% complete", 0}, {Ready, "", 0},
	} {
		if got := percent(c.st, c.detail); got != c.want {
			t.Errorf("percent(%s, %q) = %d, want %d", c.st, c.detail, got, c.want)
		}
	}
}

// fakeCache answers the guest package's Cache, recording its calls.
type fakeCache struct {
	calls int
	p     *cache.Prepared
	err   error
}

func (f *fakeCache) Ensure(_ context.Context, _ string, _ release.Release, onLine func(string)) (*cache.Prepared, error) {
	f.calls++
	if onLine != nil {
		onLine("mirroring")
	}
	return f.p, f.err
}

func mirrored() *cache.Prepared {
	return &cache.Prepared{
		Entry:      cache.Entry{Repo: "ocp/release/1111111111111111"},
		PullSecret: `{"auths":{"quay.io":{},"172.30.0.5:5000":{"auth":"QQ=="}}}`,
		Sources: []cache.ImageDigestSource{
			{Source: "quay.io/openshift-release-dev/ocp-release", Mirrors: []string{"172.30.0.5:5000/ocp/release/1111111111111111"}},
		},
		TrustBundle: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
	}
}

func TestCreateUsesTheCache(t *testing.T) {
	w := newWorld(t)
	fc := &fakeCache{p: mirrored()}
	env := w.env()
	env.Cache = fc
	if _, err := Create(context.Background(), env, createOpts("seed", "4.20")); err != nil {
		t.Fatal(err)
	}
	if fc.calls != 1 {
		t.Fatalf("cache called %d times", fc.calls)
	}
	iso := w.runner.ran("iso.sh")
	for _, want := range []string{"imageDigestSources:", "172.30.0.5:5000/ocp/release/1111111111111111",
		"additionalTrustBundle:", "172.30.0.5:5000"} {
		if !strings.Contains(iso[0].stdin, want) {
			t.Errorf("ISO tarball lacks %q", want)
		}
	}
}

func TestCreateNoCache(t *testing.T) {
	w := newWorld(t)
	fc := &fakeCache{p: mirrored()}
	env := w.env()
	env.Cache = fc
	o := createOpts("seed", "4.20")
	o.NoCache = true
	if _, err := Create(context.Background(), env, o); err != nil {
		t.Fatal(err)
	}
	if fc.calls != 0 {
		t.Errorf("cache called %d times", fc.calls)
	}
	if iso := w.runner.ran("iso.sh"); strings.Contains(iso[0].stdin, "imageDigestSources") {
		t.Error("the install config points at a mirror anyway")
	}
}

func TestCreateFailsWhenMirroringFails(t *testing.T) {
	w := newWorld(t)
	env := w.env()
	env.Cache = &fakeCache{err: errors.New("registry full")}
	_, err := Create(context.Background(), env, createOpts("seed", "4.20"))
	if err == nil || !strings.Contains(err.Error(), "registry full") || !strings.Contains(err.Error(), "--no-cache") {
		t.Errorf("got %v", err)
	}
	if len(w.runner.ran("iso.sh")) != 0 {
		t.Error("built an ISO after the mirror failed")
	}
}

// Fit counts several vSNOs at once, and says when memory is short in a way
// callers can test for.
func TestFit(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)                                                                   // 90 GiB allocatable, nothing running
	if err := Fit(ctx, w.env(), node, DefaultCores, DefaultMemoryGiB, 5); err != nil { // 5 × 17
		t.Errorf("five: %v", err)
	}
	err := Fit(ctx, w.env(), node, DefaultCores, DefaultMemoryGiB, 6)
	if !errors.Is(err, ErrNoCapacity) || !strings.Contains(err.Error(), "6 vSNOs of 16 GiB need 102 GiB") {
		t.Errorf("six: %v", err)
	}
	if err := Fit(ctx, w.env(), node, 200, DefaultMemoryGiB, 1); err == nil || errors.Is(err, ErrNoCapacity) {
		t.Errorf("too many cores is not a memory shortage: %v", err)
	}
}

// A size left at 0 is the default, so callers other than vsno create (vsno
// run) needn't know the defaults.
func TestCreateOptionsSized(t *testing.T) {
	o := CreateOptions{}.Sized()
	if o.Cores != DefaultCores || o.MemoryGiB != DefaultMemoryGiB || o.DiskGiB != DefaultDiskGiB {
		t.Errorf("%+v", o)
	}
	if o := (CreateOptions{Cores: 8, MemoryGiB: 24, DiskGiB: 150}).Sized(); o.Cores != 8 || o.MemoryGiB != 24 || o.DiskGiB != 150 {
		t.Errorf("given sizes changed: %+v", o)
	}
}
