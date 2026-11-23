package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
)

func TestCacheUsers(t *testing.T) {
	gs := []guest.Guest{
		{Name: "cached", Image: "img", Cached: true},
		{Name: "plain", Image: "img"},
		{Name: "nameless", Cached: true},
	}
	users := cacheUsers(gs)
	if len(users) != 1 || len(users["img"]) != 1 || users["img"][0] != "cached" {
		t.Errorf("users = %v", users)
	}
}

func TestRenderCache(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rep := &cache.Report{
		Entries: []cache.Entry{
			{Version: "4.20.46", Image: "quay.io/x:4.20.46", SizeBytes: 6 << 30,
				Mirrored: now.Add(-2 * time.Hour), LastUsed: now.Add(-time.Hour), Users: []string{"a", "b"}},
			{Version: "4.21.0", Image: "quay.io/x:4.21.0", SizeBytes: 3 << 30,
				Mirrored: now.Add(-48 * time.Hour), LastUsed: now.Add(-48 * time.Hour)},
			{Version: "4.21.42", Image: "quay.io/x:4.21.42", State: cache.StateMirroring, Started: now.Add(-2 * time.Minute),
				Progress: &cache.MirrorProgress{ElapsedSeconds: 120, BytesComplete: 9 << 30, EstimatedBytes: 18 << 30,
					Percent: 50, BytesPerSecond: 12 << 20, ETASeconds: 240}},
		},
		Used: 40 << 30, Total: 100 << 30,
	}
	var b bytes.Buffer
	if err := renderCache(&b, rep, now, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"VERSION", "STATE", "4.20.46", "quay.io/x:4.20.46", "6.0Gi", "a, b",
		"ready", "mirroring", "PROGRESS", "9.0Gi of ~18.0Gi (50%), 12MiB/s", "~4m0s left",
		"registry volume: 40.0Gi of 100.0Gi used, 60.0Gi free"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A release no vSNO uses shows "-", not an empty cell.
	if !strings.Contains(out, "4.21.0") {
		t.Errorf("second entry missing:\n%s", out)
	}

	b.Reset()
	if err := renderCache(&b, &cache.Report{}, now, plain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "No releases cached") {
		t.Errorf("empty report = %q", b.String())
	}
}

func TestRenderGCDryRun(t *testing.T) {
	t.Cleanup(func() { outputFormat = formatTable })
	var b bytes.Buffer
	if err := renderGCDryRun(&b, gcReport{Used: 40 << 30, Total: 100 << 30, Tagged: 36 << 30, Freeable: 4 << 30, DryRun: true}, plain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"40.0Gi of 100.0Gi", "take 36.0Gi", "free about 4.0Gi", "nothing was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	b.Reset()
	if err := renderGCDryRun(&b, gcReport{Used: 10 << 30, Total: 100 << 30, Tagged: 10 << 30, DryRun: true}, plain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "nothing to collect") {
		t.Errorf("nothing freeable: %s", b.String())
	}
	b.Reset()
	outputFormat = formatJSON
	if err := renderGCDryRun(&b, gcReport{Used: 40 << 30, Total: 100 << 30, Tagged: 36 << 30, Freeable: 4 << 30, DryRun: true}, plain); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"freeable"`, `"dryRun": true`, `"tagged"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("json missing %q: %s", want, b.String())
		}
	}
}

// gcPlanFor is the dry run's measurement: the volume's used bytes, and what the
// tagged releases and seeds add up to.
func TestGCPlanFor(t *testing.T) {
	ctx := context.Background()
	entry := cache.Entry{Version: "4.22.22", Image: "quay.io/x:4.22.22", Digest: "sha256:aa", Repo: "ocp/release/aa", SizeBytes: 30 << 30}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: cache.ConfigMap},
		Data: map[string]string{"cache.json": `{"entries":{"sha256:aa":` + mustJSON(t, entry) + `}}`}}
	k := kfake.NewClientset(cm,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "registry-auth"}, Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "registry-ca"}, Data: map[string][]byte{"ca.crt": []byte("CA")}},
	)
	env := cache.Env{Kube: k, Now: func() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) },
		Usage: func(context.Context, string) (int64, int64, error) { return 40 << 30, 100 << 30, nil }}
	genv := guest.Env{Kube: k, Builder: seedLister{}}
	rep, err := gcPlanFor(ctx, env, genv)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Used != 40<<30 || rep.Total != 100<<30 || rep.Tagged != 32<<30 || rep.Freeable != 8<<30 || !rep.DryRun {
		t.Errorf("report = %+v", rep)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedLister answers the registry listing with one seed image of two GiB.
type seedLister struct{}

func (seedLister) Run(_ context.Context, _ string, _ []string, io_ builder.IO) error {
	_, err := io_.Stdout.Write([]byte(`{"repo":"seeds/a","tag":"4.22.22","digest":"sha256:bb","created":"2026-11-23T10:00:00Z","labels":{},"size":2147483648}` + "\n"))
	return err
}

// When the tagged images add up to more than the volume — they share layers —
// the dry run says so instead of claiming there is nothing to free.
func TestRenderGCDryRunSharedLayers(t *testing.T) {
	t.Cleanup(func() { outputFormat = formatTable })
	var b bytes.Buffer
	if err := renderGCDryRun(&b, gcReport{Used: 40 << 30, Total: 100 << 30, Tagged: 60 << 30, DryRun: true}, plain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "share layers") || strings.Contains(b.String(), "nothing to collect") {
		t.Errorf("shared layers: %s", b.String())
	}
}
