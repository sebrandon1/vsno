package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/ui"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"
)

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func testListing() *listing {
	return &listing{
		VSNOs: []guest.Guest{
			{Namespace: "vsno", Name: "a", State: guest.Ready, Version: "4.20.46", Cores: 16, MemoryGiB: 32, DiskGiB: 200, ServiceIP: "172.30.1.1", Node: "worker-1", Created: now.Add(-3 * time.Hour)},
			{Namespace: "vsno-dev", Name: "b", State: guest.Installing, Detail: "installing 40%", Requested: "4.22-nightly", Cores: 8, MemoryGiB: 16, DiskGiB: 120, Created: now.Add(-10 * time.Minute)},
		},
		Capacity: []guest.Capacity{{Node: "worker-1", FreeMemoryGiB: 46, FitDefault: 1}},
	}
}

func TestRenderList(t *testing.T) {
	var b bytes.Buffer
	if err := renderList(&b, testListing(), now, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"NAMESPACE", "NAME", "STATE", "VERSION", "AGE", "CPU", "MEMORY", "DISK", "API", "DETAIL",
		"vsno", "vsno-dev", "api.a (172.30.1.1)", "4.22-nightly", "installing 40%",
		"node worker-1: 46 GiB memory free, room for 1 more default-size vSNO(s) (16 GiB each)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	b.Reset()
	_ = renderList(&b, &listing{Namespace: "vsno"}, now, plain)
	if !strings.Contains(b.String(), "No vSNOs in vsno") {
		t.Errorf("empty: %q", b.String())
	}
	b.Reset()
	_ = renderList(&b, &listing{}, now, plain)
	if !strings.Contains(b.String(), "No vSNOs in any vsno environment") {
		t.Errorf("empty all: %q", b.String())
	}
}

func TestRenderListJSON(t *testing.T) {
	outputFormat = formatJSON
	t.Cleanup(func() { outputFormat = formatTable })
	var b bytes.Buffer
	if err := renderList(&b, testListing(), now, plain); err != nil {
		t.Fatal(err)
	}
	var got listing
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || len(got.VSNOs) != 2 ||
		len(got.Capacity) != 1 || got.Capacity[0].FitDefault != 1 || got.VSNOs[1].Namespace != "vsno-dev" {
		t.Fatalf("json = %s (%v)", b.String(), err)
	}
}

func TestSize(t *testing.T) {
	if size(0, "Gi") != "-" || size(16, "Gi") != "16Gi" || size(8, "") != "8" {
		t.Error("size")
	}
}

func TestStateTextAndAge(t *testing.T) {
	for s, want := range map[guest.State]string{guest.Ready: colored.Green("ready"), guest.Failed: colored.Red("failed"),
		guest.Stopped: colored.Dim("stopped"), guest.Unknown: colored.Dim("unknown"), guest.Installing: colored.Yellow("installing")} {
		if got := stateText(colored, s); got != want {
			t.Errorf("stateText(%s) = %q", s, got)
		}
	}
	for d, want := range map[time.Duration]string{30 * time.Second: "30s", 5 * time.Minute: "5m", 30 * time.Hour: "30h", 72 * time.Hour: "3d"} {
		if got := age(d); got != want {
			t.Errorf("age(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestRenderCreated(t *testing.T) {
	g := &guest.Guest{Name: "a", Version: "4.20.46", API: "https://api.a.vsno.lab:6443", State: guest.Installing, Detail: "installing 40%"}
	var b bytes.Buffer
	_ = renderCreated(&b, g, true, plain)
	if !strings.Contains(b.String(), "✓ vSNO a is ready: OpenShift 4.20.46, https://api.a.vsno.lab:6443") || !strings.Contains(b.String(), "vsno oc a -- get nodes") {
		t.Errorf("ready:\n%s", b.String())
	}
	b.Reset()
	_ = renderCreated(&b, g, false, plain)
	if !strings.Contains(b.String(), "vSNO a is installing in vsno (about 45 minutes). Follow it with `vsno -n vsno list --watch`.") {
		t.Errorf("not ready:\n%s", b.String())
	}
	outputFormat = formatJSON
	t.Cleanup(func() { outputFormat = formatTable })
	b.Reset()
	var got guest.Guest
	if err := renderCreated(&b, g, true, plain); err != nil || json.Unmarshal(b.Bytes(), &got) != nil || got.Name != "a" {
		t.Errorf("json: %s", b.String())
	}
}

func deleteResult(actions ...apply.Action) *guest.DeleteResult {
	r := &guest.DeleteResult{Namespace: "vsno"}
	for i, a := range actions {
		r.Objects = append(r.Objects, apply.Change{Kind: "Part", Namespace: "vsno", Name: string(rune('a' + i)), Action: a})
	}
	return r
}

func TestRenderDelete(t *testing.T) {
	for _, c := range []struct {
		name      string
		res       *guest.DeleteResult
		planShown bool
		want      string
	}{
		{"plan", deleteResult(apply.Delete, apply.Absent), false, "2 part(s): 1 to delete, 1 absent. Nothing was changed."},
		{"plan already shown", deleteResult(apply.Delete), true, ""},
		{"all deleted", deleteResult(apply.Removed, apply.Absent), true, "✓ 1 part(s) deleted."},
		{"partly deleted", deleteResult(apply.Removed, apply.Delete), true, "! 1 of 2 part(s) deleted."},
		{"nothing", &guest.DeleteResult{}, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			_ = renderDelete(&b, c.res, c.planShown, plain)
			if (c.want == "" && b.Len() != 0) || !strings.Contains(b.String(), c.want) {
				t.Errorf("out = %q, want %q", b.String(), c.want)
			}
		})
	}
	dlDryRun = true
	t.Cleanup(func() { dlDryRun = false })
	var b bytes.Buffer
	renderDeletePlan(&b, deleteResult(apply.Delete), plain)
	if !strings.Contains(b.String(), "Dry run: nothing was changed.") {
		t.Errorf("dry run: %q", b.String())
	}
	outputFormat = formatJSON
	t.Cleanup(func() { outputFormat = formatTable })
	b.Reset()
	if err := renderDelete(&b, deleteResult(apply.Removed), false, plain); err != nil || !strings.Contains(b.String(), `"action": "deleted"`) {
		t.Errorf("json: %s", b.String())
	}
}

func TestFixedSize(t *testing.T) {
	f := &fixedSize{size: &remotecommand.TerminalSize{Width: 80, Height: 24}}
	if s := f.Next(); s == nil || s.Width != 80 {
		t.Errorf("first = %v", s)
	}
	if f.Next() != nil {
		t.Error("the size is reported once")
	}
}

func TestColorKeepsLayoutForGuests(t *testing.T) {
	for name, render := range map[string]func(*bytes.Buffer, ui.Palette){
		"list":   func(b *bytes.Buffer, p ui.Palette) { _ = renderList(b, testListing(), now, p) },
		"delete": func(b *bytes.Buffer, p ui.Palette) { renderDeletePlan(b, deleteResult(apply.Delete, apply.Absent), p) },
	} {
		var p, c bytes.Buffer
		render(&p, plain)
		render(&c, colored)
		if ui.StripANSI(c.String()) != p.String() || !strings.Contains(c.String(), "\033[") {
			t.Errorf("%s: layout differs with color", name)
		}
	}
}

func TestUseEnvironment(t *testing.T) {
	ns := func(name string, ours bool) *corev1.Namespace {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if ours {
			n.Labels = map[string]string{cluster.ManagedByLabel: "vsno"}
		}
		return n
	}
	t.Cleanup(func() {
		namespace = "vsno"
		_ = rootCmd.PersistentFlags().Set("namespace", "vsno")
		rootCmd.PersistentFlags().Lookup("namespace").Changed = false
	})
	for _, c := range []struct {
		name    string
		objs    []runtime.Object
		want    string
		wantErr string
	}{
		{"none: keep the default", []runtime.Object{ns("vsno", false)}, "vsno", ""},
		{"one elsewhere", []runtime.Object{ns("vsno", false), ns("vsno-dev", true)}, "vsno-dev", ""},
		{"the default is ours", []runtime.Object{ns("vsno", true), ns("vsno-dev", true)}, "vsno", ""},
		{"several", []runtime.Object{ns("b", true), ns("a", true)}, "vsno", "several vsno environments (a, b)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			namespace = "vsno"
			err := useEnvironment(context.Background(), kfake.NewClientset(c.objs...))
			if namespace != c.want || (c.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.wantErr)) {
				t.Errorf("namespace %q, err %v", namespace, err)
			}
		})
	}
	// An explicit -n wins.
	_ = rootCmd.PersistentFlags().Set("namespace", "mine")
	if err := useEnvironment(context.Background(), kfake.NewClientset(ns("vsno-dev", true))); err != nil || namespace != "mine" {
		t.Errorf("explicit: %q %v", namespace, err)
	}
	// A failed list keeps the default.
	_ = rootCmd.PersistentFlags().Set("namespace", "vsno")
	rootCmd.PersistentFlags().Lookup("namespace").Changed = false
	k := kfake.NewClientset()
	k.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("forbidden") })
	if err := useEnvironment(context.Background(), k); err != nil || namespace != "vsno" {
		t.Errorf("list error: %q %v", namespace, err)
	}
}

// A time never set is "-", not now minus year 1 (Go clamps that to 106751d).
func TestAgeSince(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if got := ageSince(now, time.Time{}); got != "-" {
		t.Errorf("zero = %q", got)
	}
	if got := ageSince(now, now.Add(-5*time.Minute)); got != "5m" {
		t.Errorf("5m ago = %q", got)
	}
}
