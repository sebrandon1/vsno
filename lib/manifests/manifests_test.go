package manifests

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cluster"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite golden files")

var all = []string{Operators, HyperConverged, LVMCluster, Namespace, DNSMasq, Builder, S3, RegistryService, Registry}

// testConfig uses synthetic values only; this repo is public.
func testConfig() Config {
	return Config{
		Namespace:    "vsno",
		Node:         "worker-1",
		DiskByID:     "/dev/disk/by-id/wwn-0x5000000000000002",
		HostDNS:      "172.30.0.10",
		CNVChannel:   "stable",
		LVMSChannel:  "stable-4.22",
		BaseDomain:   DefaultBaseDomain,
		Guests:       []string{"seed", "target"},
		StorageClass: DefaultStorageClass,
		Images:       DefaultImages(),
		S3AccessKey:  "vsno",
		S3SecretKey:  "s3-secret",
		S3Bucket:     DefaultS3Bucket,
		Registry: RegistryAuth{
			CACert: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
			CAKey:  "ca-key", Cert: "cert", Key: "key",
			User: "vsno", Password: "pw", Htpasswd: "vsno:$2a$10$hash",
		},
	}
}

func TestGolden(t *testing.T) {
	for _, name := range all {
		t.Run(name, func(t *testing.T) {
			got, err := RenderYAML(name, testConfig())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", name+".golden.yaml")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test ./lib/manifests -update)", err)
			}
			if got != string(want) {
				t.Errorf("%s differs from %s; rerun with -update and review the diff", name, path)
			}
		})
	}
}

func TestRenderParsesEveryTemplate(t *testing.T) {
	for _, name := range all {
		objs, err := Render(name, testConfig())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(objs) == 0 {
			t.Fatalf("%s rendered no objects", name)
		}
		for _, o := range objs {
			if o.GetKind() == "" || o.GetName() == "" {
				t.Errorf("%s: object without kind/name: %v", name, o.Object)
			}
		}
	}
}

func find(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no %s/%s", kind, name)
	return nil
}

func TestDNSMasqHasNoWildcard(t *testing.T) {
	objs, err := Render(DNSMasq, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	conf, _, _ := unstructured.NestedString(find(t, objs, "ConfigMap", "dnsmasq").Object, "data", "dnsmasq.conf")
	for _, bad := range []string{"address=/vsno.lab/", "address=/seed.vsno.lab/", "address=/target.vsno.lab/"} {
		if strings.Contains(conf, bad) {
			t.Errorf("dnsmasq.conf has a wildcard %q:\n%s", bad, conf)
		}
	}
	for _, want := range []string{
		"address=/api.seed.vsno.lab/10.0.2.2",
		"address=/api-int.target.vsno.lab/10.0.2.2",
		"address=/apps.target.vsno.lab/10.0.2.2",
		"server=172.30.0.10",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("dnsmasq.conf missing %q", want)
		}
	}
	if n := strings.Count(conf, "address="); n != 6 {
		t.Errorf("want 6 address lines (3 per guest), got %d", n)
	}
}

func TestS3Manifests(t *testing.T) {
	objs, err := Render(S3, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	sec := find(t, objs, "Secret", "s3-config")
	js, _, _ := unstructured.NestedString(sec.Object, "stringData", "s3.json")
	if !strings.Contains(js, `"accessKey":"vsno"`) || !strings.Contains(js, `"secretKey":"s3-secret"`) {
		t.Errorf("s3.json does not carry the generated key: %s", js)
	}
	sc, _, _ := unstructured.NestedString(find(t, objs, "PersistentVolumeClaim", "s3-data").Object, "spec", "storageClassName")
	if sc != DefaultStorageClass {
		t.Errorf("s3-data storageClassName = %q", sc)
	}
	ports, _, _ := unstructured.NestedSlice(find(t, objs, "Service", "s3").Object, "spec", "ports")
	if len(ports) != 1 || ports[0].(map[string]any)["port"] != int64(9000) {
		t.Errorf("s3 Service ports = %v", ports)
	}
	dep, _ := yamlOf(find(t, objs, "Deployment", "s3"))
	if !strings.Contains(dep, "s3.bucket.create -name ibu") || strings.Contains(dep, ":latest") {
		t.Errorf("s3 Deployment should create bucket ibu with a pinned image:\n%s", dep)
	}
}

func TestRegistryManifests(t *testing.T) {
	cfg := testConfig()
	objs, err := Render(Registry, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ca, _, _ := unstructured.NestedString(find(t, objs, "Secret", "registry-ca").Object, "stringData", "ca.crt")
	if ca != cfg.Registry.CACert {
		t.Errorf("multi-line PEM did not round-trip: %q", ca)
	}
	if typ, _, _ := unstructured.NestedString(find(t, objs, "Secret", "registry-tls").Object, "type"); typ != "kubernetes.io/tls" {
		t.Errorf("registry-tls type = %q", typ)
	}
	ht, _, _ := unstructured.NestedString(find(t, objs, "Secret", "registry-auth").Object, "stringData", "htpasswd")
	if ht != cfg.Registry.Htpasswd {
		t.Errorf("htpasswd = %q", ht)
	}
	sc, _, _ := unstructured.NestedString(find(t, objs, "PersistentVolumeClaim", "registry-data").Object, "spec", "storageClassName")
	if sc != DefaultStorageClass {
		t.Errorf("registry-data storageClassName = %q", sc)
	}
	svc, err := Render(RegistryService, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ports, _, _ := unstructured.NestedSlice(svc[0].Object, "spec", "ports")
	if len(ports) != 1 || ports[0].(map[string]any)["port"] != int64(5000) {
		t.Errorf("registry Service ports = %v", ports)
	}
}

func TestLVMClusterUsesNodeAndDisk(t *testing.T) {
	s, err := RenderYAML(LVMCluster, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"/dev/disk/by-id/wwn-0x5000000000000002"`, `values: ["worker-1"]`} {
		if !strings.Contains(s, want) {
			t.Errorf("lvmcluster missing %s", want)
		}
	}
}

func TestQuoteEscapesInjection(t *testing.T) {
	cfg := testConfig()
	cfg.LVMSChannel = "stable\nmalicious: true"
	objs, err := Render(Operators, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sub := find(t, objs, "Subscription", "lvms")
	if ch, _, _ := unstructured.NestedString(sub.Object, "spec", "channel"); ch != cfg.LVMSChannel {
		t.Errorf("channel = %q", ch)
	}
	if _, ok := sub.Object["spec"].(map[string]any)["malicious"]; ok {
		t.Error("value injected a key")
	}
}

func TestRenderUnknownTemplate(t *testing.T) {
	if _, err := Render("nope", testConfig()); err == nil {
		t.Fatal("want error")
	}
}

func yamlOf(o *unstructured.Unstructured) (string, error) {
	b, err := yaml.Marshal(o.Object)
	return string(b), err
}

func TestRenderLabelsEveryObject(t *testing.T) {
	for _, name := range []string{Operators, HyperConverged, LVMCluster, Namespace, DNSMasq, Builder, RegistryService, S3, Registry} {
		objs, err := Render(name, testConfig())
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			if !cluster.ManagedByVsno(o.GetLabels()) {
				t.Errorf("%s: %s/%s has no managed-by=vsno label", name, o.GetKind(), o.GetName())
			}
		}
	}
}

func TestStepsObjects(t *testing.T) {
	want := map[string][]string{
		"cnv":            {"Namespace/openshift-cnv", "OperatorGroup/kubevirt-hyperconverged-group", "Subscription/hco-operatorhub"},
		"lvms":           {"Namespace/openshift-storage", "OperatorGroup/openshift-storage-operatorgroup", "Subscription/lvms"},
		"hyperconverged": {"HyperConverged/kubevirt-hyperconverged"},
		"storage":        {"LVMCluster/vsno-lvmcluster"},
		"namespace":      {"Namespace/vsno-x", "ServiceAccount/vsno", "RoleBinding/vsno-anyuid"},
	}
	total := 0
	for _, s := range Steps {
		objs, err := s.Objects(NamesConfig("vsno-x"))
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		total += len(objs)
		var got []string
		for _, o := range objs {
			got = append(got, o.GetKind()+"/"+o.GetName())
			if !s.Host && o.GetKind() != "Namespace" && o.GetNamespace() != "vsno-x" {
				t.Errorf("%s: %s is outside the environment namespace", s.Name, o.GetName())
			}
		}
		if w, ok := want[s.Name]; ok && !slices.Equal(got, w) {
			t.Errorf("%s = %v, want %v", s.Name, got, w)
		}
	}
	if total != 27 {
		t.Errorf("%d objects in all steps, want 27 (19 in the namespace, 8 on the host)", total)
	}
}
