package setup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/pki"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	ns       = "vsno-test"
	node     = "worker-1"
	disk     = "/dev/disk/by-id/wwn-0x5000000000000002"
	regIP    = "172.30.0.50"
	cnvCSV   = "kubevirt-hyperconverged-operator.v1.0.0"
	lvmsCSV  = "lvms-operator.v1.0.0"
	scName   = manifests.DefaultStorageClass
	topolvm  = "topolvm.io"
	hostDNS  = "172.30.0.10"
	channel1 = "stable"
)

var gvrs = map[string]schema.GroupVersionResource{
	"Namespace":             {Version: "v1", Resource: "namespaces"},
	"ServiceAccount":        {Version: "v1", Resource: "serviceaccounts"},
	"ConfigMap":             {Version: "v1", Resource: "configmaps"},
	"Secret":                cluster.GVRSecret,
	"Service":               cluster.GVRService,
	"PersistentVolumeClaim": {Version: "v1", Resource: "persistentvolumeclaims"},
	"Deployment":            cluster.GVRDeployment,
	"RoleBinding":           {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	"OperatorGroup":         {Group: "operators.coreos.com", Version: "v1", Resource: "operatorgroups"},
	"Subscription":          {Group: "operators.coreos.com", Version: "v1alpha1", Resource: "subscriptions"},
	"HyperConverged":        precheck.GVRHyperConverged,
	"LVMCluster":            precheck.GVRLVMCluster,
}

// sim is an Applier that stores objects in fake clients and fills in the
// status a real cluster would report, so the waits succeed.
type sim struct {
	kube    *kfake.Clientset
	dyn     *dynfake.FakeDynamicClient
	applied []string // Kind/name in order
	ips     map[string]string
	// noCSV leaves Subscriptions without a CSV, so the wait times out.
	noCSV bool
}

func newSim(objs ...runtime.Object) *sim {
	s := &sim{kube: kfake.NewClientset(), ips: map[string]string{"registry": regIP}}
	s.dyn = dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		precheck.GVRCSV:        "ClusterServiceVersionList",
		precheck.GVRLVMCluster: "LVMClusterList",
		cluster.GVRSecret:      "SecretList",
	}, objs...)
	return s
}

func (s *sim) Apply(_ context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	var ch []apply.Change
	for _, o := range objs {
		o = o.DeepCopy()
		s.applied = append(s.applied, o.GetKind()+"/"+o.GetName())
		s.simulate(o)
		gvr, ok := gvrs[o.GetKind()]
		if !ok {
			return ch, errors.New("sim: unknown kind " + o.GetKind())
		}
		c := apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), Action: apply.Created}
		tr := s.dyn.Tracker()
		cur, err := tr.Get(gvr, o.GetNamespace(), o.GetName())
		switch {
		case err != nil:
			err = tr.Create(gvr, o, o.GetNamespace())
		case reflect.DeepEqual(cur.(*unstructured.Unstructured).Object, o.Object):
			c.Action = apply.Unchanged
		default:
			c.Action = apply.Updated
			err = tr.Update(gvr, o, o.GetNamespace())
		}
		if err != nil {
			return ch, err
		}
		ch = append(ch, c)
	}
	return ch, nil
}

// simulate adds what controllers and the API server would.
func (s *sim) simulate(o *unstructured.Unstructured) {
	set := func(v any, f ...string) { _ = unstructured.SetNestedField(o.Object, v, f...) }
	switch o.GetKind() {
	case "Secret":
		sd, _, _ := unstructured.NestedStringMap(o.Object, "stringData")
		data := map[string]any{}
		for k, v := range sd {
			data[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		delete(o.Object, "stringData")
		set(data, "data")
	case "Service":
		ip := s.ips[o.GetName()]
		if ip == "" {
			ip = "172.30.0.99"
		}
		set(ip, "spec", "clusterIP")
	case "Deployment":
		set(int64(1), "status", "updatedReplicas")
		set(int64(1), "status", "availableReplicas")
	case "HyperConverged":
		set([]any{map[string]any{"type": "Available", "status": "True"}}, "status", "conditions")
	case "LVMCluster":
		set("Ready", "status", "state")
		_, _ = s.kube.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: scName}}, metav1.CreateOptions{})
		_, _ = s.kube.StorageV1().CSINodes().Create(context.Background(), csiNode(), metav1.CreateOptions{})
	case "Subscription":
		if !s.noCSV {
			name := cnvCSV
			if o.GetNamespace() == storageNS {
				name = lvmsCSV
			}
			_ = s.dyn.Tracker().Create(precheck.GVRCSV, csv(o.GetNamespace(), name, "Succeeded"), o.GetNamespace())
		}
	}
}

func csv(namespace, name, phase string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"phase": phase}}}
	u.SetAPIVersion("operators.coreos.com/v1alpha1")
	u.SetKind("ClusterServiceVersion")
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func csiNode() *storagev1.CSINode {
	return &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: node},
		Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: topolvm, NodeID: node}}}}
}

// existingHost makes the sim look like a cluster with CNV, LVMS and vsno storage.
func (s *sim) existingHost(t *testing.T) {
	t.Helper()
	tr := s.dyn.Tracker()
	for _, o := range []*unstructured.Unstructured{csv(cnvNS, cnvCSV, "Succeeded"), csv(storageNS, lvmsCSV, "Succeeded")} {
		if err := tr.Create(precheck.GVRCSV, o, o.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
	hc := &unstructured.Unstructured{Object: map[string]any{}}
	hc.SetAPIVersion("hco.kubevirt.io/v1beta1")
	hc.SetKind("HyperConverged")
	hc.SetNamespace(cnvNS)
	hc.SetName(hcoName)
	s.simulate(hc)
	if err := tr.Create(precheck.GVRHyperConverged, hc, cnvNS); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.kube.StorageV1().StorageClasses().Create(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: scName}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.kube.StorageV1().CSINodes().Create(ctx, csiNode(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func report(results ...precheck.Result) *precheck.Report {
	return &precheck.Report{Results: results, Suggested: precheck.Suggested{
		Node: node, Disk: disk, DiskSizeGB: 1000, HostDNS: hostDNS, CNVChannel: channel1, LVMSChannel: channel1,
	}}
}

func (s *sim) env(rep *precheck.Report) Env {
	return Env{
		Kube: s.kube, Dyn: s.dyn, Applier: s,
		Precheck: func(context.Context, string) *precheck.Report { return rep },
		Now:      func() time.Time { return time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC) },
		Poll:     time.Millisecond, Timeout: 50 * time.Millisecond,
	}
}

func opts() Options { return Options{Namespace: ns, Yes: true, Images: manifests.DefaultImages()} }

func outcomes(res *Result) map[string]Outcome {
	m := map[string]Outcome{}
	for _, s := range res.Steps {
		m[s.Name] = s.Outcome
	}
	return m
}

func TestRunFreshCluster(t *testing.T) {
	s := newSim()
	res, err := Run(context.Background(), s.env(report()), opts())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Outcome{"cnv": Applied, "lvms": Applied, "hyperconverged": Applied, "storage": Applied, "namespace": Applied, "support": Applied}
	if got := outcomes(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes = %v", got)
	}
	if res.Registry != net.JoinHostPort(regIP, "5000") {
		t.Errorf("registry = %q", res.Registry)
	}
	if apply.Count(res.Changes, apply.Created) != len(res.Changes) {
		t.Errorf("fresh run should only create: %v", res.Changes)
	}

	st, found, err := state.Load(context.Background(), s.kube, ns)
	if err != nil || !found {
		t.Fatalf("state: %v %v", found, err)
	}
	if st.Node != node || st.Disk != disk || st.Versions["cnv"] != cnvCSV || st.Versions["lvms"] != lvmsCSV || st.Services["registry"] != regIP {
		t.Errorf("state = %+v", st)
	}
	if last, _ := st.Last(StageName); last.Status != state.Done {
		t.Errorf("last stage = %+v", last)
	}

	// The registry cert covers its ClusterIP and Service names, signed by the stored CA.
	ca, _ := (&runner{Env: s.env(nil), o: opts()}).secret(context.Background(), "registry-ca")
	tls, _ := (&runner{Env: s.env(nil), o: opts()}).secret(context.Background(), "registry-tls")
	if !pki.Covers([]byte(tls["tls.crt"]), []byte(ca["ca.crt"]), []net.IP{net.ParseIP(regIP)}, []string{"registry." + ns + ".svc"}, time.Now()) {
		t.Error("registry cert does not cover the ClusterIP")
	}
}

func TestRunReusesHostAndRerunIsNoOp(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	o := opts()
	o.Yes = false // no disk prompt when storage is reused
	res, err := Run(context.Background(), s.env(report(precheck.Result{Check: node + ": memory", Status: precheck.Fail, Detail: "8 GiB schedulable"})), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "8 GiB") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	got := outcomes(res)
	for _, n := range []string{"cnv", "lvms", "hyperconverged", "storage"} {
		if got[n] != Reused {
			t.Errorf("%s = %s, want reused", n, got[n])
		}
	}
	for _, a := range s.applied {
		if strings.HasPrefix(a, "Subscription/") || strings.HasPrefix(a, "LVMCluster/") || strings.HasPrefix(a, "HyperConverged/") {
			t.Errorf("applied %s on a cluster that has it", a)
		}
	}
	first := s.secrets(t)

	res, err = Run(context.Background(), s.env(report()), o)
	if err != nil {
		t.Fatal(err)
	}
	if n := apply.Count(res.Changes, apply.Unchanged); n != len(res.Changes) {
		t.Errorf("rerun changed objects: %v", res.Changes)
	}
	if !reflect.DeepEqual(first, s.secrets(t)) {
		t.Error("rerun regenerated credentials")
	}
	st, _, _ := state.Load(context.Background(), s.kube, ns)
	if st.Disk != "" || len(st.Stages) != 2 {
		t.Errorf("state = %+v", st)
	}
}

func TestRunWaitsForExistingHyperConverged(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	tr := s.dyn.Tracker()
	cur, err := tr.Get(precheck.GVRHyperConverged, cnvNS, hcoName)
	if err != nil {
		t.Fatal(err)
	}
	hc := cur.(*unstructured.Unstructured)
	_ = unstructured.SetNestedSlice(hc.Object, []any{map[string]any{"type": "Available", "status": "False"}}, "status", "conditions")
	if err := tr.Update(precheck.GVRHyperConverged, hc, cnvNS); err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), s.env(report()), opts())
	if err == nil || !strings.Contains(err.Error(), "waiting for HyperConverged Available") {
		t.Fatalf("want a wait for Available, got %v", err)
	}
	o := opts()
	o.DryRun = true
	if _, err := Run(context.Background(), s.env(report()), o); err != nil {
		t.Errorf("dry run should not wait: %v", err)
	}
}

func (s *sim) secrets(t *testing.T) map[string]map[string]string {
	t.Helper()
	r := &runner{Env: s.env(nil), o: opts()}
	out := map[string]map[string]string{}
	for _, n := range []string{"s3-config", "registry-ca", "registry-tls", "registry-auth"} {
		d, err := r.secret(context.Background(), n)
		if err != nil || d == nil {
			t.Fatalf("secret %s: %v %v", n, d, err)
		}
		out[n] = d
	}
	return out
}

func TestRunReissuesCertWhenIPChanges(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	ctx := context.Background()
	if _, err := Run(ctx, s.env(report()), opts()); err != nil {
		t.Fatal(err)
	}
	before := s.secrets(t)

	s.ips["registry"] = "172.30.0.51" // Service recreated with a new IP
	res, err := Run(ctx, s.env(report()), opts())
	if err != nil {
		t.Fatal(err)
	}
	after := s.secrets(t)
	if before["registry-tls"]["tls.crt"] == after["registry-tls"]["tls.crt"] {
		t.Error("cert was not reissued for the new IP")
	}
	if before["registry-ca"]["ca.crt"] != after["registry-ca"]["ca.crt"] || !reflect.DeepEqual(before["registry-auth"], after["registry-auth"]) {
		t.Error("CA and credentials must survive a reissue")
	}
	if res.Registry != "172.30.0.51:5000" {
		t.Errorf("registry = %q", res.Registry)
	}
}

func TestRunRegeneratesBrokenCredentials(t *testing.T) {
	stale := &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{
		"username": base64.StdEncoding.EncodeToString([]byte("vsno")),
		"password": base64.StdEncoding.EncodeToString([]byte("new")),
		"htpasswd": base64.StdEncoding.EncodeToString([]byte("vsno:$2y$05$doesnotmatch")),
	}}}
	stale.SetAPIVersion("v1")
	stale.SetKind("Secret")
	stale.SetNamespace(ns)
	stale.SetName("registry-auth")
	s := newSim(stale)
	s.existingHost(t)
	if _, err := Run(context.Background(), s.env(report()), opts()); err != nil {
		t.Fatal(err)
	}
	a := s.secrets(t)["registry-auth"]
	if a["password"] == "new" || !pki.CheckHtpasswd(a["htpasswd"], a["username"], a["password"]) {
		t.Errorf("mismatched htpasswd was not regenerated: %v", a)
	}
}

func TestRunDiskConfirmation(t *testing.T) {
	cases := []struct {
		name    string
		confirm func(q, a string) bool
		wantErr string
	}{
		{"no prompt available", nil, "not confirmed"},
		{"declined", func(string, string) bool { return false }, "not confirmed"},
		{"typed node name", func(q, a string) bool { return a == node && strings.Contains(q, disk) }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSim()
			e := s.env(report())
			e.Confirm = c.confirm
			o := opts()
			o.Yes = false
			_, err := Run(context.Background(), e, o)
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v", err)
			}
			for _, a := range s.applied {
				if strings.HasPrefix(a, "LVMCluster/") {
					t.Error("LVMCluster applied without confirmation")
				}
			}
		})
	}
}

func TestRunStorageErrors(t *testing.T) {
	ctx := context.Background()
	other := &unstructured.Unstructured{Object: map[string]any{}}
	other.SetAPIVersion("lvm.topolvm.io/v1alpha1")
	other.SetKind("LVMCluster")
	other.SetNamespace(storageNS)
	other.SetName("someone-elses")

	t.Run("StorageClass without TopoLVM on the node", func(t *testing.T) {
		s := newSim()
		_, _ = s.kube.StorageV1().StorageClasses().Create(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: scName}}, metav1.CreateOptions{})
		_, err := Run(ctx, s.env(report()), opts())
		if err == nil || !strings.Contains(err.Error(), "does not serve node "+node) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("another LVMCluster", func(t *testing.T) {
		s := newSim(other)
		_, err := Run(ctx, s.env(report()), opts())
		if err == nil || !strings.Contains(err.Error(), "supports only one") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no disk", func(t *testing.T) {
		s := newSim()
		rep := report()
		rep.Suggested.Disk = ""
		_, err := Run(ctx, s.env(rep), opts())
		if err == nil || !strings.Contains(err.Error(), "pass --disk") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRunWaitTimesOutAndRecordsNothingBeforeNamespace(t *testing.T) {
	s := newSim()
	s.noCSV = true
	res, err := Run(context.Background(), s.env(report()), opts())
	if err == nil || !strings.Contains(err.Error(), "waiting for kubevirt-hyperconverged-operator.* CSV") {
		t.Fatalf("err = %v", err)
	}
	if len(res.Steps) != 0 {
		t.Errorf("steps = %v", res.Steps)
	}
	if _, found, _ := state.Load(context.Background(), s.kube, ns); found {
		t.Error("state saved before the namespace existed")
	}
}

func TestRunRecordsFailureAfterNamespace(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	e := s.env(report())
	e.Timeout = 5 * time.Millisecond
	// Deployments never become available.
	s.dyn.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("not yet")
	})
	_, err := Run(context.Background(), e, opts())
	if err == nil || !strings.Contains(err.Error(), "support Deployments") || !strings.Contains(err.Error(), "not yet") {
		t.Fatalf("err = %v", err)
	}
	st, found, _ := state.Load(context.Background(), s.kube, ns)
	if !found {
		t.Fatal("no state")
	}
	if last, _ := st.Last(StageName); last.Status != state.Failed || !strings.Contains(last.Detail, "Support services") {
		t.Errorf("last = %+v", last)
	}
}

func TestRunMissingChannel(t *testing.T) {
	s := newSim()
	rep := report()
	rep.Suggested.CNVChannel = ""
	_, err := Run(context.Background(), s.env(rep), opts())
	if err == nil || !strings.Contains(err.Error(), "no update channel for kubevirt-hyperconverged-operator") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunDryRun(t *testing.T) {
	s := newSim()
	o := opts()
	o.DryRun, o.Yes = true, false
	res, err := Run(context.Background(), s.env(report()), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.applied) != 0 {
		t.Fatalf("dry run applied %v", s.applied)
	}
	for _, st := range res.Steps {
		if st.Outcome != Planned {
			t.Errorf("%s = %s", st.Name, st.Outcome)
		}
	}
	if _, found, _ := state.Load(context.Background(), s.kube, ns); found {
		t.Error("dry run saved state")
	}
	secrets := 0
	for _, obj := range res.Objects {
		if obj.GetKind() != "Secret" {
			continue
		}
		secrets++
		sd, _, _ := unstructured.NestedStringMap(obj.Object, "stringData")
		for k, v := range sd {
			if v != "<redacted>" {
				t.Errorf("%s %s = %q, want redacted", obj.GetName(), k, v)
			}
		}
	}
	if secrets != 4 {
		t.Errorf("dry run rendered %d secrets, want 4", secrets)
	}
}

func TestResolve(t *testing.T) {
	fail := func(check string) precheck.Result {
		return precheck.Result{Check: check, Status: precheck.Fail, Detail: "x"}
	}
	cases := []struct {
		name      string
		results   []precheck.Result
		user      precheck.Suggested
		suggested string
		wantErr   string
		wantWarn  int
		wantNode  string
		wantDisk  string
	}{
		{name: "clean", suggested: node, wantNode: node, wantDisk: disk},
		{name: "cluster failure stops", results: []precheck.Result{fail("Network overlap")}, suggested: node, wantErr: "Network overlap"},
		{name: "catalog failure stops", results: []precheck.Result{fail("Catalog: LVM Storage")}, suggested: node, wantErr: "Catalog: LVM Storage"},
		{name: "memory on the chosen node warns",
			results: []precheck.Result{fail("worker-2: memory"), fail("Eligible node")},
			user:    precheck.Suggested{Node: "worker-2"}, wantNode: "worker-2", wantWarn: 1},
		{name: "kvm on the chosen node stops",
			results: []precheck.Result{fail("worker-2: virtualization")},
			user:    precheck.Suggested{Node: "worker-2"}, wantErr: "worker-2: virtualization"},
		{name: "other nodes are ignored", results: []precheck.Result{fail("worker-9: disk")}, suggested: node, wantNode: node, wantDisk: disk},
		{name: "no node", results: []precheck.Result{fail("Eligible node")}, wantErr: "precheck failed: Eligible node"},
		{name: "no probe, no node", wantErr: "no node selected"},
		{name: "user disk wins", user: precheck.Suggested{Disk: "/dev/disk/by-id/wwn-0x5000000000000009"}, suggested: node,
			wantNode: node, wantDisk: "/dev/disk/by-id/wwn-0x5000000000000009"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := &precheck.Report{Results: c.results}
			if c.suggested != "" {
				rep.Suggested = precheck.Suggested{Node: c.suggested, Disk: disk, HostDNS: hostDNS}
			}
			s, warns, err := resolve(rep, c.user)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Node != c.wantNode || s.Disk != c.wantDisk || len(warns) != c.wantWarn {
				t.Errorf("got node=%q disk=%q warnings=%v", s.Node, s.Disk, warns)
			}
		})
	}
}

func TestRedactLeavesOtherKinds(t *testing.T) {
	cm := &unstructured.Unstructured{Object: map[string]any{"kind": "ConfigMap", "data": map[string]any{"k": "v"}}}
	if redact(cm) != cm {
		t.Error("non-Secret objects should pass through")
	}
	sec := &unstructured.Unstructured{Object: map[string]any{"kind": "Secret", "data": map[string]any{"k": "djE="}}}
	r := redact(sec)
	if v, _, _ := unstructured.NestedString(r.Object, "data", "k"); v != "<redacted>" {
		t.Errorf("data not redacted: %v", r.Object)
	}
	if v, _, _ := unstructured.NestedString(sec.Object, "data", "k"); v != "djE=" {
		t.Error("redact modified the original")
	}
}

func TestRunStopsOnCrashLoop(t *testing.T) {
	s := newSim()
	s.existingHost(t)
	e := s.env(report())
	e.Timeout = time.Minute // must not be reached
	s.dyn.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		d := &unstructured.Unstructured{Object: map[string]any{}}
		d.SetName(a.(k8stesting.GetAction).GetName())
		return true, d, nil // never available
	})
	crash := func(restarts int32) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("builder-%d", restarts), Namespace: ns, Labels: map[string]string{"app": "builder"}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{RestartCount: restarts,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}},
		}
	}
	ctx := context.Background()
	if _, err := s.kube.CoreV1().Pods(ns).Create(ctx, crash(crashLoopRestarts-1), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond) // first polls see too few restarts
		_, _ = s.kube.CoreV1().Pods(ns).Create(ctx, crash(crashLoopRestarts), metav1.CreateOptions{})
	}()
	start := time.Now()
	_, err := Run(ctx, e, opts())
	if err == nil || !strings.Contains(err.Error(), "builder-3 keeps crashing (3 restarts)") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("crash loop did not end the wait early")
	}
}

func TestRunAPIErrors(t *testing.T) {
	boom := func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("api down") }
	cases := []struct {
		name, verb, resource string
		host                 bool // CNV/LVMS/storage already present
		ips                  map[string]string
		wantErr              string
	}{
		{name: "listing CSVs", verb: "list", resource: "clusterserviceversions", wantErr: "api down"},
		{name: "getting HyperConverged", verb: "get", resource: "hyperconvergeds", host: true, wantErr: "api down"},
		{name: "listing LVMClusters", verb: "list", resource: "lvmclusters", wantErr: "api down"},
		{name: "reading secrets", verb: "get", resource: "secrets", host: true, wantErr: "reading secret s3-config: api down"},
		{name: "registry without ClusterIP", host: true, ips: map[string]string{"registry": "None"}, wantErr: `service registry has no ClusterIP ("None")`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSim()
			if c.host {
				s.existingHost(t)
			}
			if c.ips != nil {
				s.ips = c.ips
			}
			if c.verb != "" {
				s.dyn.PrependReactor(c.verb, c.resource, boom)
			}
			_, err := Run(context.Background(), s.env(report()), opts())
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}
