package guest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Kinds of a vSNO's parts that aren't Kubernetes objects of their own.
const (
	KindDNS   = "DNS entry"     // a key in the guests ConfigMap
	KindFiles = "Builder files" // the ISO, install dir and kubeconfig in the builder
)

// DeleteOptions are the user's choices for vsno delete.
type DeleteOptions struct {
	Namespace string
	Names     []string
	DryRun    bool
	Yes       bool
	// Confirm asks the user to type answer to approve question. Nil never approves.
	Confirm func(question, answer string) bool
	// ShowPlan, if set, is called with the plan right before Confirm.
	ShowPlan func(*DeleteResult)
}

// DeleteResult lists each part of each vSNO and what happened to it.
type DeleteResult struct {
	Namespace string         `json:"namespace"`
	Objects   []apply.Change `json:"objects"`
}

// part is one thing a vSNO consists of, in deletion order.
type part struct {
	kind, name string
	gvr        schema.GroupVersionResource // empty for DNS and files
}

func parts(name string) []part {
	return []part{
		{"VirtualMachine", name, cluster.GVRVirtualMachine},
		{"DataVolume", name + "-root", GVRDataVolume},
		{"DataVolume", name + "-iso", GVRDataVolume},
		{"Service", name, cluster.GVRService},
		{KindDNS, name + ".conf", schema.GroupVersionResource{}},
		{KindFiles, "/work/guests/" + name, schema.GroupVersionResource{}},
	}
}

// Delete removes the named vSNOs: VM, disks, ISO, Service, DNS entry and the
// builder's files. It refuses a name with nothing to delete.
func Delete(ctx context.Context, e Env, o DeleteOptions) (*DeleteResult, error) {
	e.defaults()
	res := &DeleteResult{Namespace: o.Namespace}
	for _, name := range o.Names {
		ch, err := planDelete(ctx, e, o.Namespace, name)
		if err != nil {
			return res, err
		}
		res.Objects = append(res.Objects, ch...)
	}
	if o.DryRun {
		return res, nil
	}
	if !o.Yes {
		answer := strings.Join(o.Names, " ")
		if o.ShowPlan != nil {
			o.ShowPlan(res)
		}
		q := fmt.Sprintf("This deletes %d vSNO(s) and their disks. Type %q to continue", len(o.Names), answer)
		if o.Confirm == nil || !o.Confirm(q, answer) {
			return res, errors.New("not confirmed; rerun with --yes to skip the question")
		}
	}
	for _, name := range o.Names {
		if err := deleteOne(ctx, e, o.Namespace, name, res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// planDelete marks each part of name delete or absent.
func planDelete(ctx context.Context, e Env, ns, name string) ([]apply.Change, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	dns, err := e.Kube.CoreV1().ConfigMaps(ns).Get(ctx, manifests.GuestsConfigMap, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading the DNS entries: %w", err)
	}
	var out []apply.Change
	found := false
	for _, p := range parts(name) {
		c := apply.Change{Kind: p.kind, Namespace: ns, Name: p.name, Action: apply.Absent}
		if p.kind == KindFiles {
			c.Namespace = "" // a path in the builder, not a namespaced object
		}
		switch p.kind {
		case KindDNS:
			if _, ok := dns.Data[p.name]; ok {
				c.Action = apply.Delete
			}
		case KindFiles:
			c.Action = apply.Delete // cheap to remove; not worth an exec to check
		default:
			_, err := e.Dyn.Resource(p.gvr).Namespace(ns).Get(ctx, p.name, metav1.GetOptions{})
			switch {
			case err == nil:
				c.Action, found = apply.Delete, true
			case !apierrors.IsNotFound(err):
				return nil, fmt.Errorf("reading %s %s: %w", p.kind, p.name, err)
			}
		}
		out = append(out, c)
	}
	if !found && !slices.ContainsFunc(out, func(c apply.Change) bool { return c.Kind == KindDNS && c.Action == apply.Delete }) {
		return nil, fmt.Errorf("no vSNO named %s in %s (see `vsno list`)", name, ns)
	}
	return out, nil
}

// deleteOne deletes name's parts and waits until its VM and disks are gone.
func deleteOne(ctx context.Context, e Env, ns, name string, res *DeleteResult) error {
	ps := e.Progress.Start("Deleting " + name)
	mark := func(kind, n string) {
		for i, c := range res.Objects {
			if c.Kind == kind && c.Name == n && c.Action == apply.Delete {
				res.Objects[i].Action = apply.Removed
			}
		}
	}
	var waits []part
	for _, p := range parts(name) {
		var err error
		switch p.kind {
		case KindDNS:
			err = SetDNS(ctx, e, ns, name, false)
		case KindFiles:
			if e.Builder != nil {
				err = e.Builder.Run(ctx, script("rm.sh"), []string{name}, builder.IO{})
			}
		default:
			err = e.Dyn.Resource(p.gvr).Namespace(ns).Delete(ctx, p.name, metav1.DeleteOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			waits = append(waits, p)
		}
		if err != nil {
			ps.Fail(err.Error())
			return fmt.Errorf("deleting %s %s: %w", p.kind, p.name, err)
		}
		mark(p.kind, p.name)
	}
	ps.Update("waiting until the VM and its disks are gone")
	err := wait.PollUntilContextTimeout(ctx, e.Poll, e.Timeout, true, func(ctx context.Context) (bool, error) {
		for _, p := range waits {
			if _, err := e.Dyn.Resource(p.gvr).Namespace(ns).Get(ctx, p.name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		ps.Fail(err.Error())
		return fmt.Errorf("waiting for vSNO %s to be deleted: %w", name, err)
	}
	ps.Done("deleted")
	return nil
}

// SetRunning starts (run) or stops a vSNO by setting its VM's runStrategy.
func SetRunning(ctx context.Context, e Env, ns, name string, run bool) error {
	if _, err := lookup(ctx, e, ns, name); err != nil {
		return err
	}
	strategy := "Halted"
	if run {
		strategy = "Always"
	}
	patch := fmt.Sprintf(`{"spec":{"runStrategy":%q}}`, strategy)
	_, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Patch(ctx, name, mergePatch, []byte(patch), metav1.PatchOptions{})
	return err
}

// WaitState polls until name reaches one of want, with progress.
func WaitState(ctx context.Context, e Env, ns, name string, want ...State) (State, error) {
	e.defaults()
	ps := e.Progress.Start("Waiting for " + name)
	var st State
	err := wait.PollUntilContextTimeout(ctx, e.Poll, e.Timeout, true, func(ctx context.Context) (bool, error) {
		var detail string
		var err error
		st, detail, _, err = observeOne(ctx, e, ns, name)
		ps.Update(string(st) + " " + detail)
		return slices.Contains(want, st), err
	})
	if err != nil {
		ps.Fail(err.Error())
		return st, err
	}
	ps.Done(string(st))
	return st, nil
}
