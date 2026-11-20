package isos

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/guest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDelete(t *testing.T) {
	t.Run("an orphaned vSNO and its DataVolume", testDeleteVSNO)
	t.Run("one unused installer, leaving the others", testDeleteInstaller)
	t.Run("an orphaned DataVolume by name", testDeleteDataVolume)
	t.Run("each argument once", testDeleteEachOnce)
	t.Run("dry run changes nothing", testDeleteDryRun)
	t.Run("asks first", testDeleteAsks)
}

func testDeleteVSNO(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: listing}
	env := testEnv(t, r)
	res, err := Delete(ctx, env, DeleteOptions{Namespace: ns, Args: []string{"gone"}, Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	// Its served ISO and build directory, counted once, plus its DataVolume.
	want := []string{"DataVolume gone-iso", "vSNO gone"}
	if !slices.Equal(res.Deleted, want) || len(res.Plan.VSNOs) != 1 {
		t.Errorf("deleted %q (plan %+v)", res.Deleted, res.Plan)
	}
	if want := int64(1395864371 + 1073741824 + 3<<30); res.Bytes != want {
		t.Errorf("bytes = %d, want %d", res.Bytes, want)
	}
	if !slices.Equal(r.ran, []string{"list", "rm-vsno gone"}) {
		t.Errorf("ran %q", r.ran)
	}
	if _, err := env.Dyn.Resource(guest.GVRDataVolume).Namespace(ns).Get(ctx, "gone-iso", metav1.GetOptions{}); err == nil {
		t.Error("the DataVolume is still there")
	}
}

func testDeleteInstaller(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: listing}
	res, err := Delete(ctx, testEnv(t, r), DeleteOptions{Namespace: ns, Args: []string{"openshift-install-bbbb"}, Yes: true})
	if err != nil || !slices.Equal(res.Deleted, []string{"installer openshift-install-bbbb"}) {
		t.Fatalf("deleted %q, %v", res.Deleted, err)
	}
	if !slices.Equal(r.ran, []string{"list", "rm-installer openshift-install-bbbb"}) {
		t.Errorf("ran %q", r.ran)
	}
}

func testDeleteDataVolume(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: listing}
	res, err := Delete(ctx, testEnv(t, r), DeleteOptions{Namespace: ns, Args: []string{"gone-iso"}, Yes: true})
	if err != nil || !slices.Equal(res.Deleted, []string{"DataVolume gone-iso"}) {
		t.Fatalf("deleted %q, %v", res.Deleted, err)
	}
}

func testDeleteEachOnce(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: listing}
	res, err := Delete(ctx, testEnv(t, r), DeleteOptions{Namespace: ns, Args: []string{"gone", "gone"}, Yes: true})
	if err != nil || len(res.Deleted) != 2 {
		t.Errorf("deleted %q, %v", res.Deleted, err)
	}
}

func testDeleteDryRun(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: listing}
	res, err := Delete(ctx, testEnv(t, r), DeleteOptions{Namespace: ns, Args: []string{"gone"}, DryRun: true})
	if err != nil || len(res.Deleted) != 0 || len(r.ran) != 1 {
		t.Errorf("deleted %q, ran %q, %v", res.Deleted, r.ran, err)
	}
}

func testDeleteAsks(t *testing.T) {
	ctx := context.Background()
	r := &runner{listing: listing}
	var asked, answer string
	_, err := Delete(ctx, testEnv(t, r), DeleteOptions{Namespace: ns, Args: []string{"gone"},
		Confirm: func(q, a string) bool { asked, answer = q, a; return false }})
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Errorf("got %v", err)
	}
	if !strings.HasPrefix(asked, "Are you sure?") || answer != "y" || len(r.ran) != 1 {
		t.Errorf("asked %q [%s], ran %q", asked, answer, r.ran)
	}
}

func TestDeleteRefusesWhatIsInUse(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ arg, want string }{
		{"a", "vsno iso eject a"},
		{"a-iso", "vsno iso eject a"},
		{"openshift-install-aaaa", "vSNO a runs"},
	} {
		t.Run(c.arg, func(t *testing.T) {
			r := &runner{listing: listing}
			_, err := Delete(ctx, testEnv(t, r), DeleteOptions{Namespace: ns, Args: []string{c.arg}, Yes: true})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
			if !slices.Equal(r.ran, []string{"list"}) {
				t.Errorf("deleted anyway: %q", r.ran)
			}
		})
	}
	t.Run("unknown", func(t *testing.T) {
		r := &runner{listing: listing}
		_, err := Delete(ctx, testEnv(t, r), DeleteOptions{Namespace: ns, Args: []string{"zzz"}, Yes: true})
		if err == nil || !strings.Contains(err.Error(), "nothing named zzz") {
			t.Errorf("got %v", err)
		}
	})
}
