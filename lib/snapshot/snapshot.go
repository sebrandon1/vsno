// Package snapshot keeps a vSNO's installed root disk as a VolumeSnapshot and
// restores a vSNO from it, so testing does not install OpenShift again.
//
// The disk carries the cluster's identity (node name, api.<name>.<domain> and
// its certificates, clusterID), so a restored disk only runs as the vSNO it was
// taken from: snapshots are kept per vSNO name and restored under that name.
//
// LVMS's volumes are thin, so the storage driver restores a snapshot as a thin
// clone in seconds. (CDI's DataVolume snapshot source copies the volume
// host-assisted instead — minutes for a few GiB — which is why a restore here is
// a PVC with a VolumeSnapshot dataSource, not a DataVolume.)
package snapshot

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// LabelVSNO is the vSNO a snapshot was taken from; its name is the
	// snapshot's.
	LabelVSNO = "vsno.io/snapshot"
	// Dir in the builder holds a copy of each vSNO's installer files, so a
	// restore can probe the guest after `vsno delete` removed them.
	Dir = "/work/snapshots"
)

// Entry is one snapshot.
type Entry struct {
	Name      string    `json:"name"`
	VSNO      string    `json:"vsno"`
	Version   string    `json:"version"`
	SizeBytes int64     `json:"sizeBytes"`
	Created   time.Time `json:"created"`
	// InUse is true when the vSNO it was taken from still exists: it is that
	// vSNO's disk, not spare space.
	InUse bool `json:"inUse,omitempty"`
}

// Env is the guest package's, plus the builder the files are kept in.
type Env struct {
	guest.Env
}

// Defaults fills in what e leaves unset: silent progress, a 10s poll, half an
// hour's timeout and the real clock.
func (e *Env) Defaults() {
	if e.Progress == nil {
		e.Progress = precheck.NoProgress()
	}
	if e.Poll == 0 {
		e.Poll = 10 * time.Second
	}
	if e.Timeout == 0 {
		e.Timeout = 30 * time.Minute
	}
	if e.Now == nil {
		e.Now = time.Now
	}
}

// List reports the environment's snapshots, newest first.
func List(ctx context.Context, e Env, ns string) ([]Entry, error) {
	ps := e.Progress.Start("Listing snapshots")
	l, err := e.Dyn.Resource(cluster.GVRVolumeSnapshot).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: cluster.ManagedByLabel + "=vsno",
	})
	if apierrors.IsNotFound(err) {
		ps.Warn("snapshot.storage.k8s.io is not installed")
		return nil, nil
	}
	if err != nil {
		ps.Fail(err.Error())
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}
	names, err := vsnoNames(ctx, e, ns) // one listing, not one per snapshot
	if err != nil {
		ps.Fail(err.Error())
		return nil, err
	}
	out := make([]Entry, 0, len(l.Items))
	for i := range l.Items {
		s := &l.Items[i]
		out = append(out, entry(s, slices.Contains(names, s.GetLabels()[LabelVSNO])))
	}
	slices.SortFunc(out, func(a, b Entry) int { return b.Created.Compare(a.Created) })
	ps.Done(fmt.Sprintf("%d snapshot(s)", len(out)))
	return out, nil
}

// Use is the snapshot a vSNO is restored from, or nil when there is none.
func Use(ctx context.Context, e Env, ns, vsnoName string) (*Entry, error) {
	u, err := e.Dyn.Resource(cluster.GVRVolumeSnapshot).Namespace(ns).Get(ctx, vsnoName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading snapshot %s: %w", vsnoName, err)
	}
	if ready, _, _ := unstructured.NestedBool(u.Object, "status", "readyToUse"); !ready {
		return nil, fmt.Errorf("snapshot %s is not ready yet (see `vsno snapshot list`)", vsnoName)
	}
	return ptr(entry(u, true)), nil
}

// CreateOptions are the user's choices for vsno snapshot.
type CreateOptions struct {
	Namespace string
	// Name is the vSNO to snapshot; empty picks the only one there is.
	Name string
}

// Create snapshots the vSNO's root disk. The vSNO is stopped for the duration:
// a snapshot of a running guest needs the guest agent to freeze its filesystems,
// which the agent cannot do inside a vSNO.
func Create(ctx context.Context, e Env, o CreateOptions) (*Entry, error) {
	e.Defaults()
	name, err := pick(ctx, e, o)
	if err != nil {
		return nil, err
	}
	wasRunning, err := runs(ctx, e, o.Namespace, name)
	if err != nil {
		return nil, err
	}
	if err := stopFor(ctx, e, o.Namespace, name); err != nil {
		return nil, err
	}
	ps := e.Progress.Start("Snapshotting " + name)
	v, err := version(ctx, e, o.Namespace, name)
	if err != nil {
		ps.Fail(err.Error())
		return nil, err
	}
	// An existing snapshot of that name is replaced: applying to it would keep
	// the old content, and a snapshot is a point in time.
	if err := replace(ctx, e, o.Namespace, name); err != nil {
		ps.Fail(err.Error())
		return nil, err
	}
	snap := snapshotObject(o.Namespace, name, v)
	if _, err := e.Applier.Apply(ctx, []*unstructured.Unstructured{snap}); err != nil {
		ps.Fail(err.Error())
		return nil, fmt.Errorf("creating the snapshot: %w", err)
	}
	ps.Update("waiting for the storage driver")
	ent, err := wait(ctx, e, o.Namespace, name)
	if err != nil {
		ps.Fail(err.Error())
		return nil, err
	}
	// The installer's files go with the disk: `vsno delete` removes the
	// originals, and a restore needs the kubeconfig to probe the guest.
	if e.Builder != nil {
		if err := e.Builder.Run(ctx, saveFiles, []string{name}, builder.IO{}); err != nil {
			ps.Warn("snapshot taken, but its installer files were not kept: " + err.Error())
			return &ent, nil
		}
	}
	if wasRunning {
		if err := guest.SetRunning(ctx, e.Env, o.Namespace, name, true); err != nil {
			ps.Warn("snapshot taken, but " + name + " was not started again: " + err.Error())
			return &ent, nil
		}
	}
	ps.Done(fmt.Sprintf("%s, %s", ent.Name, bytesText(ent.SizeBytes)))
	return &ent, nil
}

// runs reports whether the vSNO was running, so Create can put it back.
func runs(ctx context.Context, e Env, ns, name string) (bool, error) {
	gs, err := guest.List(ctx, e.Env, ns)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(gs, func(g guest.Guest) bool { return g.Name == name })
	if i < 0 {
		return false, fmt.Errorf("no vSNO named %s in %s (see `vsno list`)", name, ns)
	}
	return gs[i].State != guest.Stopped, nil
}

// DeleteOptions are the user's choices for `vsno snapshot delete`.
type DeleteOptions struct {
	Namespace string
	Names     []string
	DryRun    bool
	// Yes skips the question; Confirm asks it.
	Yes     bool
	Confirm func(question, answer string) bool
	// ShowPlan lists what would be deleted, before the question.
	ShowPlan func(Deleted)
}

// Deleted is what a delete removed, or would.
type Deleted struct {
	Snapshots []Entry `json:"snapshots"`
}

// Empty reports whether there is nothing to delete.
func (d Deleted) Empty() bool { return len(d.Snapshots) == 0 }

// Delete removes the named snapshots and the files kept with them.
func Delete(ctx context.Context, e Env, o DeleteOptions) (Deleted, error) {
	e.Defaults()
	var out Deleted
	all, err := List(ctx, e, o.Namespace)
	if err != nil {
		return out, err
	}
	for _, n := range o.Names {
		i := slices.IndexFunc(all, func(en Entry) bool { return en.Name == n })
		if i < 0 {
			return out, fmt.Errorf("no snapshot named %s (see `vsno snapshot list`)", n)
		}
		out.Snapshots = append(out.Snapshots, all[i])
	}
	if out.Empty() || o.DryRun {
		return out, nil
	}
	if !o.Yes && (o.Confirm == nil || !o.Confirm(fmt.Sprintf("Delete %d snapshot(s)?", len(out.Snapshots)), "yes")) {
		return out, fmt.Errorf("not confirmed; pass --yes to delete")
	}
	for _, s := range out.Snapshots {
		if err := e.Dyn.Resource(cluster.GVRVolumeSnapshot).Namespace(o.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return out, fmt.Errorf("deleting snapshot %s: %w", s.Name, err)
		}
		if e.Builder != nil {
			_ = e.Builder.Run(ctx, removeFiles, []string{s.Name}, builder.IO{})
		}
	}
	return out, nil
}

// pick is the vSNO to snapshot: the one named, or the only one there is.
func pick(ctx context.Context, e Env, o CreateOptions) (string, error) {
	gs, err := guest.List(ctx, e.Env, o.Namespace)
	if err != nil {
		return "", err
	}
	if o.Name != "" {
		if !slices.ContainsFunc(gs, func(g guest.Guest) bool { return g.Name == o.Name }) {
			return "", fmt.Errorf("no vSNO named %s in %s (see `vsno list`)", o.Name, o.Namespace)
		}
		return o.Name, nil
	}
	var names []string
	for _, g := range gs {
		names = append(names, g.Name)
	}
	switch len(names) {
	case 0:
		return "", fmt.Errorf("no vSNO in %s to snapshot", o.Namespace)
	case 1:
		return names[0], nil
	}
	slices.Sort(names)
	return "", fmt.Errorf("several vSNOs in %s; name the one to snapshot: %v", o.Namespace, names)
}

// stopFor stops the vSNO, and leaves it stopped: a snapshot is only taken of a
// vSNO that is not running, and Create starts it again.
func stopFor(ctx context.Context, e Env, ns, name string) error {
	ps := e.Progress.Start("Stopping " + name)
	if err := guest.SetRunning(ctx, e.Env, ns, name, false); err != nil {
		ps.Fail(err.Error())
		return err
	}
	if st, err := guest.WaitState(ctx, e.Env, ns, name, guest.Stopped); err != nil {
		ps.Fail(err.Error())
		return fmt.Errorf("waiting for %s to stop (%s): %w", name, st, err)
	}
	ps.Done("stopped")
	return nil
}

// replace removes the snapshot of that name, if any, so Create takes a new one.
func replace(ctx context.Context, e Env, ns, name string) error {
	rs := e.Dyn.Resource(cluster.GVRVolumeSnapshot).Namespace(ns)
	if err := rs.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("replacing snapshot %s: %w", name, err)
	}
	return retryLoop(ctx, e, func(ctx context.Context) (bool, error) {
		if _, err := rs.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, nil
	})
}

// version is the OpenShift version the vSNO's disk holds, as `vsno list` shows it.
func version(ctx context.Context, e Env, ns, name string) (string, error) {
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading vSNO %s: %w", name, err)
	}
	ann := vm.GetAnnotations()
	if v := ann[guest.AnnVersion]; v != "" {
		return v, nil
	}
	return ann[guest.AnnRequested], nil
}

// snapshotObject is the VolumeSnapshot for a vSNO's root disk.
func snapshotObject(ns, name, version string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"volumeSnapshotClassName": manifests.DefaultStorageClass,
			"source":                  map[string]any{"persistentVolumeClaimName": name + "-root"},
		},
	}}
	u.SetAPIVersion("snapshot.storage.k8s.io/v1")
	u.SetKind("VolumeSnapshot")
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(map[string]string{cluster.ManagedByLabel: "vsno", LabelVSNO: name})
	u.SetAnnotations(map[string]string{guest.AnnVersion: version})
	return u
}

// wait returns the snapshot once the storage driver reports it usable.
func wait(ctx context.Context, e Env, ns, name string) (Entry, error) {
	var ent Entry
	err := retryLoop(ctx, e, func(ctx context.Context) (bool, error) {
		u, err := e.Dyn.Resource(cluster.GVRVolumeSnapshot).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil // a dropped read is not a failed snapshot
		}
		if msg, _, _ := unstructured.NestedString(u.Object, "status", "error", "message"); msg != "" {
			return false, fmt.Errorf("the storage driver failed to snapshot %s: %s", name, msg)
		}
		ready, _, _ := unstructured.NestedBool(u.Object, "status", "readyToUse")
		if ready {
			ent = entry(u, true)
			return true, nil
		}
		return false, nil
	})
	return ent, err
}

// retryLoop polls fn until it says done, the context ends, or the timeout does.
func retryLoop(ctx context.Context, e Env, fn func(context.Context) (bool, error)) error {
	deadline := e.Now().Add(e.Timeout)
	for {
		done, err := fn(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if e.Now().After(deadline) {
			return fmt.Errorf("not ready after %s", e.Timeout)
		}
		select {
		case <-time.After(e.Poll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// entry reads one VolumeSnapshot into an Entry.
func entry(u *unstructured.Unstructured, inUse bool) Entry {
	e := Entry{
		Name:    u.GetName(),
		VSNO:    u.GetLabels()[LabelVSNO],
		Version: u.GetAnnotations()[guest.AnnVersion],
		Created: u.GetCreationTimestamp().Time,
		InUse:   inUse,
	}
	if size, _, _ := unstructured.NestedString(u.Object, "status", "restoreSize"); size != "" {
		if q, err := resource.ParseQuantity(size); err == nil {
			e.SizeBytes = q.Value()
		}
	}
	return e
}

// vsnoNames are the vSNOs in the namespace.
func vsnoNames(ctx context.Context, e Env, ns string) ([]string, error) {
	gs, err := guest.List(ctx, e.Env, ns)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out, nil
}

// Scripts in the builder: the installer files kept next to the disk.
const (
	saveFiles   = `rm -rf "/work/snapshots/$1" && mkdir -p "/work/snapshots" && cp -a "/work/guests/$1" "/work/snapshots/$1"`
	removeFiles = `rm -rf "/work/snapshots/$1"`
)

// bytesText renders a size in GiB or MiB.
func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	default:
		return fmt.Sprintf("%.0fMi", float64(n)/(1<<20))
	}
}

func ptr[T any](v T) *T { return &v }
