package guestctl_test

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/guestctl/guestctltest"
	"github.com/sebrandon1/vsno/lib/precheck"
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
		t.Fatalf("%v (run go test ./lib/guestctl -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file (go test ./lib/guestctl -update):\n%s", name, got)
	}
}

var (
	lca  = guestctl.Operator{Name: "LCA", Package: "lifecycle-agent", CSVPrefix: "lifecycle-agent.", Namespace: "openshift-lifecycle-agent", Management: true}
	oadp = guestctl.Operator{Name: "OADP", Package: "redhat-oadp-operator", CSVPrefix: "oadp-operator.", Namespace: "openshift-adp"}
)

// LCA's manifests are what seed rendered before they were shared, so guests
// that already run LCA keep their one OperatorGroup.
func TestOperatorManifests(t *testing.T) {
	for _, c := range []struct {
		op   guestctl.Operator
		file string
	}{{lca, "lca.yaml.golden"}, {oadp, "oadp.yaml.golden"}} {
		y, err := c.op.Manifests("stable", "redhat-operators", "openshift-marketplace")
		if err != nil {
			t.Fatal(err)
		}
		golden(t, c.file, y)
	}
}

func TestOperatorCSV(t *testing.T) {
	ctx := context.Background()
	f := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{
		"get clusterserviceversions -n openshift-adp --ignore-not-found": {{Out: `{"items":[
			{"metadata":{"name":"lifecycle-agent.v4.21.5"},"status":{"phase":"Succeeded"}},
			{"metadata":{"name":"oadp-operator.v1.5.2"},"status":{"phase":"Installing"}}]}`}},
	}}
	if name, ok, err := oadp.CSV(ctx, f); err != nil || name != "oadp-operator.v1.5.2" || ok {
		t.Errorf("csv = %q succeeded = %v err = %v", name, ok, err)
	}
	if name, _, err := lca.CSV(ctx, &guestctltest.Fake{}); err != nil || name != "" {
		t.Errorf("not installed: csv = %q err = %v", name, err)
	}
}

func TestOperatorEnsure(t *testing.T) {
	ctx := context.Background()
	csvs := "get clusterserviceversions -n openshift-adp --ignore-not-found"
	catalog := "get packagemanifests redhat-oadp-operator -n openshift-marketplace --ignore-not-found"
	installing := guestctltest.Reply{Out: `{"items":[{"metadata":{"name":"oadp-operator.v1.5.2"},"status":{"phase":"Installing"}}]}`}
	done := guestctltest.Reply{Out: `{"items":[{"metadata":{"name":"oadp-operator.v1.5.2"},"status":{"phase":"Succeeded"}}]}`}
	pm := guestctltest.Reply{Out: `{"status":{"catalogSource":"redhat-operators","catalogSourceNamespace":"openshift-marketplace","defaultChannel":"stable"}}`}
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	c := guestctl.Clock{Every: time.Millisecond, Now: func() time.Time { now = now.Add(time.Minute); return now }}
	ps := precheck.NoProgress().Start("x")

	t.Run("installs and waits", func(t *testing.T) {
		f := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{csvs: {{Out: `{"items":[]}`}, installing, done}, catalog: {pm}}}
		name, err := oadp.Ensure(ctx, f, c, time.Hour, ps)
		if err != nil || name != "oadp-operator.v1.5.2" || !f.Ran("oc apply -f -") || !strings.Contains(f.Stdin[0], "channel: \"stable\"") {
			t.Errorf("name = %q err = %v calls = %q", name, err, f.Calls)
		}
	})
	t.Run("already installed", func(t *testing.T) {
		f := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{csvs: {done}}}
		if _, err := oadp.Ensure(ctx, f, c, time.Hour, ps); err != nil || f.Ran("oc apply") || f.Ran("oc "+catalog) {
			t.Errorf("err = %v calls = %q", err, f.Calls)
		}
	})
	t.Run("not in the catalog", func(t *testing.T) {
		f := &guestctltest.Fake{}
		if _, err := oadp.Ensure(ctx, f, c, time.Hour, ps); err == nil || !strings.Contains(err.Error(), "no redhat-oadp-operator package") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("never succeeds", func(t *testing.T) {
		f := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{csvs: {installing}}}
		if _, err := oadp.Ensure(ctx, f, c, 5*time.Minute, ps); err == nil || !strings.Contains(err.Error(), "OADP wasn't installed after 5m0s") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("catalog read fails", func(t *testing.T) {
		f := &guestctltest.Fake{OcReplies: map[string][]guestctltest.Reply{catalog: {{Err: errors.New("exit 1")}}}}
		if _, err := oadp.Ensure(ctx, f, c, time.Hour, ps); err == nil || !strings.Contains(err.Error(), "packagemanifests") {
			t.Errorf("err = %v", err)
		}
	})
}
