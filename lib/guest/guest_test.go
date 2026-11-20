package guest

import (
	"archive/tar"
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/release"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func testSpec() Spec {
	return Spec{Namespace: "vsno", Name: "seed", Node: "worker-1", Cores: 16, MemoryGiB: 32, DiskGiB: 200,
		Release: release.Release{Requested: "4.20", Version: "4.20.46", Image: "quay.io/openshift-release-dev/ocp-release:4.20.46-x86_64"}}
}

var testSecrets = Secrets{PullSecret: `{"auths":{"quay.io":{"auth":"dGVzdA=="}}}`, SSHPublicKey: "ssh-ed25519 AAAAtest vsno@vsno"}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./lib/guest -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file (go test ./lib/guest -update):\n%s", name, got)
	}
}

func TestInstallFilesGolden(t *testing.T) {
	files, err := InstallFiles(testSpec(), "172.30.0.53", testSecrets)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %v", files)
	}
	for path, content := range files {
		golden(t, strings.ReplaceAll(path, "/", "_")+".golden", content)
		var v any
		if err := yaml.Unmarshal([]byte(content), &v); err != nil {
			t.Errorf("%s is not valid YAML: %v", path, err)
		}
	}
	mc := files["openshift/98-var-lib-containers-partitioned.yaml"]
	if !strings.Contains(mc, `systemd-fsck@dev-disk-by\x2dpartlabel-var\x2dlib\x2dcontainers.service`) {
		t.Error("the partition unit must use systemd escaping (lifecycle-agent#10670)")
	}
}

func TestObjectsGolden(t *testing.T) {
	objs, err := Objects(testSpec(), "172.30.0.53", testSecrets)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var b strings.Builder
	for _, o := range objs {
		kinds = append(kinds, o.GetKind())
		if o.GetLabels()[LabelName] != "seed" || o.GetLabels()["app.kubernetes.io/managed-by"] != "vsno" {
			t.Errorf("%s labels = %v", o.GetKind(), o.GetLabels())
		}
		y, err := yaml.Marshal(o.Object)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("---\n")
		b.Write(y)
	}
	if strings.Join(kinds, ",") != "Service,DataVolume,VirtualMachine" {
		t.Errorf("kinds = %v (Create relies on this order)", kinds)
	}
	golden(t, "objects.golden.yaml", b.String())
}

func TestObjectsForPullspec(t *testing.T) {
	s := testSpec()
	s.Release = release.Release{Requested: "quay.io/x/release@sha256:abc", Image: "quay.io/x/release@sha256:abc"}
	objs, err := Objects(s, "172.30.0.53", testSecrets)
	if err != nil {
		t.Fatal(err)
	}
	ann := objs[2].GetAnnotations()
	if _, ok := ann[AnnVersion]; ok || ann[AnnImage] != s.Release.Image {
		t.Errorf("a pullspec has no version until installed: %v", ann)
	}
}

func TestValidate(t *testing.T) {
	for name, wantErr := range map[string]string{
		"seed": "", "a": "", "vsno-1": "", strings.Repeat("a", 30): "",
		strings.Repeat("a", 31): "invalid name", "1abc": "invalid name", "Seed": "invalid name", "a-": "invalid name", "a_b": "invalid name", "": "invalid name",
		"registry": "reserved", "builder": "reserved",
	} {
		err := ValidateName(name)
		if (wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), wantErr)) {
			t.Errorf("ValidateName(%q) = %v, want %q", name, err, wantErr)
		}
	}
	for _, c := range []struct {
		mod  func(*Spec)
		want string
	}{
		{func(*Spec) {}, ""},
		{func(s *Spec) { s.Name = "Bad" }, "invalid name"},
		{func(s *Spec) { s.Cores = 7 }, "--cores 7"},
		{func(s *Spec) { s.MemoryGiB = 15 }, "--memory 15"},
		{func(s *Spec) { s.DiskGiB = 100 }, "--disk 100"},
	} {
		s := testSpec()
		c.mod(&s)
		err := s.Validate()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("Validate = %v, want %q", err, c.want)
		}
	}
}

func TestMAC(t *testing.T) {
	a, b := MAC("vsno", "seed"), MAC("vsno", "target")
	if !regexp.MustCompile(`^02:76:73(:[0-9a-f]{2}){3}$`).MatchString(a) || a == b || a != MAC("vsno", "seed") {
		t.Errorf("MAC: %s %s", a, b)
	}
}

func TestContainersStart(t *testing.T) {
	for disk, want := range map[int64]int64{200: 102400, 500: 102400, 120: 61440} {
		if got := containersStart(disk); got != want {
			t.Errorf("containersStart(%d) = %d, want %d", disk, got, want)
		}
	}
}

func TestTar(t *testing.T) {
	b, err := Tar(map[string]string{"b/two": "2", "a": "1"})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	var got []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		got = append(got, h.Name+"="+string(c))
	}
	if strings.Join(got, ",") != "a=1,b/two=2" {
		t.Errorf("tar = %v", got)
	}
}

func TestScripts(t *testing.T) {
	for _, s := range []string{"iso.sh", "probe.sh", "rm.sh"} {
		if script(s) == "" {
			t.Errorf("%s is empty", s)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("a missing script must panic")
		}
	}()
	script("missing.sh")
}

func TestInstallConfigWithMirror(t *testing.T) {
	sec := testSecrets
	sec.ImageDigestSources = []cache.ImageDigestSource{{
		Source:  "quay.io/openshift-release-dev/ocp-release",
		Mirrors: []string{"172.30.0.50:5000/ocp/release/1a2b3c4d5e6f7788"},
	}}
	sec.AdditionalTrustBundle = "-----BEGIN CERTIFICATE-----\nMIID\n-----END CERTIFICATE-----\n"
	files, err := InstallFiles(testSpec(), "172.30.0.53", sec)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "install-config-mirror.yaml.golden", files["install-config.yaml"])
}
