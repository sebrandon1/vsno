package seed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/precheck"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CreateOptions are the user's choices for vsno seed create.
type CreateOptions struct {
	Namespace string
	Name      string // the vSNO to make the seed from
	Image     string // "" = Ref in the environment's registry
	Stop      bool   // stop the vSNO afterwards
}

// Result is a finished (or failed) seed generation.
type Result struct {
	Name     string        `json:"name"`
	Image    string        `json:"image"`
	Version  string        `json:"version"`
	LCA      string        `json:"lca,omitempty"` // the LCA CSV on the vSNO
	Duration time.Duration `json:"duration"`      // of the seed generation itself
	Stopped  bool          `json:"stopped,omitempty"`
}

// Limits for the steps before and after the seed generation (e.Timeout).
const (
	stableWait = 15 * time.Minute
	lcaWait    = 10 * time.Minute
	trustWait  = 10 * time.Minute
)

// Create makes a seed image from vSNO o.Name: it checks the cluster is
// stable, installs LCA from the vSNO's catalog if needed, makes the vSNO
// trust the environment's registry, then runs LCA's seed generation and
// follows it until the image is pushed and the cluster is back.
func Create(ctx context.Context, e Env, o CreateOptions) (*Result, error) {
	e.Defaults(o.Namespace)
	res := &Result{Name: o.Name, Image: o.Image}
	var reg Registry
	if err := guestctl.Step(e.Progress, "Checking "+o.Name, func(precheck.ProgressStep) (string, error) {
		rep, err := Precheck(ctx, e, PrecheckOptions{Namespace: o.Namespace, Name: o.Name, Image: o.Image, Settle: true})
		if err != nil {
			return "", err
		}
		res.Version, res.Image, res.LCA, reg = rep.Version, rep.Image, rep.LCA, rep.Registry
		return res.Version + ", " + res.Image, failure(rep)
	}); err != nil {
		return res, err
	}
	a := e.Access(o.Name)
	if err := guestctl.Step(e.Progress, "Lifecycle Agent", func(ps precheck.ProgressStep) (string, error) {
		var err error
		res.LCA, err = ensureLCA(ctx, e, a, ps)
		return res.LCA, err
	}); err != nil {
		return res, err
	}
	if imageHost(res.Image) == reg.Host {
		if err := guestctl.Step(e.Progress, "Trusting the environment registry", func(ps precheck.ProgressStep) (string, error) {
			return reg.Host, EnsureTrust(ctx, e, a, reg, ps)
		}); err != nil {
			return res, err
		}
	}
	auth, err := seedAuth(ctx, e.Kube, reg, res.Image)
	if err != nil {
		return res, err
	}
	if err := guestctl.Step(e.Progress, "Generating the seed image", func(ps precheck.ProgressStep) (string, error) {
		start := e.Now()
		err := generate(ctx, e, a, res.Image, auth, ps)
		res.Duration = e.Now().Sub(start).Round(time.Second)
		return fmt.Sprintf("%s in %s", res.Image, res.Duration), err
	}); err != nil {
		return res, err
	}
	finish(ctx, e, a, o, res)
	return res, nil
}

// unstable lists why the cluster isn't settled: ClusterOperators not
// Available, Progressing or Degraded, and MachineConfigPools not Updated.
func unstable(cos, mcps guestctl.ObjectList) []string {
	var out []string
	for _, co := range cos.Items {
		switch {
		case !co.IsTrue("Available"):
			out = append(out, co.Metadata.Name+" unavailable")
		case co.IsTrue("Degraded"):
			out = append(out, co.Metadata.Name+" degraded")
		case co.IsTrue("Progressing"):
			out = append(out, co.Metadata.Name+" progressing")
		}
	}
	for _, p := range mcps.Items {
		switch {
		case p.IsTrue("Degraded"):
			out = append(out, "MachineConfigPool "+p.Metadata.Name+" degraded")
		case !p.IsTrue("Updated") || p.IsTrue("Updating"):
			out = append(out, "MachineConfigPool "+p.Metadata.Name+" updating")
		}
	}
	if len(cos.Items) == 0 {
		out = append(out, "no ClusterOperators reported")
	}
	return out
}

// waitStable waits for unstable to be empty, up to stableWait.
func waitStable(ctx context.Context, e Env, a Access, ps precheck.ProgressStep) error {
	var last []string
	err := poll(ctx, e, stableWait, func() (bool, error) {
		var cos, mcps guestctl.ObjectList
		if _, err := guestctl.Get(ctx, a, &cos, "get", "clusteroperators"); err != nil {
			return false, err
		}
		if _, err := guestctl.Get(ctx, a, &mcps, "get", "machineconfigpools"); err != nil {
			return false, err
		}
		last = unstable(cos, mcps)
		ps.Update(strings.Join(last, ", "))
		return len(last) == 0, nil
	})
	if errors.Is(err, guestctl.ErrTimeout) {
		return fmt.Errorf("the cluster didn't settle in %s: %s", stableWait, strings.Join(last, ", "))
	}
	return err
}

// Clock is how e's waits poll: every e.Poll, by e.Now.
func (e Env) Clock() guestctl.Clock { return guestctl.Clock{Every: e.Poll, Now: e.Now} }

// poll calls fn every e.Poll until it's done, fails, or limit passes.
func poll(ctx context.Context, e Env, limit time.Duration, fn func() (bool, error)) error {
	return guestctl.Poll(ctx, e.Clock(), limit, fn)
}

// LCA is the Lifecycle Agent, which makes seed images and runs upgrades.
var LCA = guestctl.Operator{Name: "LCA", Package: "lifecycle-agent", CSVPrefix: "lifecycle-agent.", Namespace: LCANamespace, Management: true}

// ensureLCA installs LCA from the vSNO's own catalog unless it's there.
func ensureLCA(ctx context.Context, e Env, a Access, ps precheck.ProgressStep) (string, error) {
	return LCA.Ensure(ctx, a, e.Clock(), lcaWait, ps)
}

// imageConfig is the part of the cluster image configuration vsno reads.
type imageConfig struct {
	Spec struct {
		AdditionalTrustedCA struct {
			Name string `json:"name"`
		} `json:"additionalTrustedCA"`
	} `json:"spec"`
}

// EnsureTrust makes the vSNO trust the registry's CA, through
// image.config's additionalTrustedCA, and waits until the node has it.
func EnsureTrust(ctx context.Context, e Env, a Access, r Registry, ps precheck.ProgressStep) error {
	y, err := render("trust.yaml.tmpl", renderData{TrustKey: r.TrustKey(), CA: r.CA})
	if err != nil {
		return err
	}
	if err := guestctl.Apply(ctx, a, y); err != nil {
		return err
	}
	var img imageConfig
	if _, err := guestctl.Get(ctx, a, &img, "get", "image.config.openshift.io", "cluster"); err != nil {
		return err
	}
	switch img.Spec.AdditionalTrustedCA.Name {
	case trustConfigMap:
	case "":
		patch := fmt.Sprintf(`{"spec":{"additionalTrustedCA":{"name":%q}}}`, trustConfigMap)
		if err := guestctl.Oc(ctx, a, "", "patch", "image.config.openshift.io", "cluster", "--type", "merge", "-p", patch); err != nil {
			return err
		}
	default:
		return fmt.Errorf("the vSNO already trusts CAs from ConfigMap openshift-config/%s; add key %s with the registry CA to it",
			img.Spec.AdditionalTrustedCA.Name, r.TrustKey())
	}
	file := certFile(r)
	ps.Update("waiting for " + file)
	err = poll(ctx, e, trustWait, func() (bool, error) {
		return guestctl.SSH(ctx, a, "test", "-f", file) == nil, nil
	})
	if errors.Is(err, guestctl.ErrTimeout) {
		return fmt.Errorf("the vSNO doesn't have %s after %s", file, trustWait)
	}
	return err
}

// seedgenPhase is where a SeedGenerator is, from its conditions.
type seedgenPhase int

const (
	sgRunning seedgenPhase = iota
	sgCompleted
	sgFailed
)

func phaseOf(sg guestctl.Object) (seedgenPhase, string) {
	done, inProgress := sg.Cond("SeedGenCompleted"), sg.Cond("SeedGenInProgress")
	switch {
	case done != nil && done.Reason == "Failed":
		return sgFailed, done.Message
	case inProgress != nil && inProgress.Reason == "Failed":
		return sgFailed, inProgress.Message
	case done != nil && done.Status == "True":
		return sgCompleted, done.Message
	case inProgress != nil:
		return sgRunning, inProgress.Message
	}
	return sgRunning, "starting"
}

// journalCmd prints the seed generation's latest log line on the node.
var journalCmd = []string{"sudo", "journalctl", "-u", "lca-generate-seed-image", "-n", "1", "-o", "cat", "--no-pager"}

// generate replaces any finished SeedGenerator with one for image, then
// follows it. The vSNO's API is down for most of it, while LCA builds the
// image on the node; meanwhile progress comes from the node's journal.
func generate(ctx context.Context, e Env, a Access, image, auth string, ps precheck.ProgressStep) error {
	var sg guestctl.Object
	found, err := guestctl.Get(ctx, a, &sg, "get", "seedgenerators", "seedimage", "--ignore-not-found")
	if err != nil {
		return err
	}
	if found {
		if p, msg := phaseOf(sg); p == sgRunning {
			return fmt.Errorf("a seed generation is already running on the vSNO: %s", msg)
		}
		if err := guestctl.Oc(ctx, a, "", "delete", "seedgenerators", "seedimage", "--wait"); err != nil {
			return err
		}
	}
	y, err := render("seedgen.yaml.tmpl", renderData{SeedAuth: auth, Image: image})
	if err != nil {
		return err
	}
	if err := guestctl.Apply(ctx, a, y); err != nil {
		return err
	}
	var failure error
	err = poll(ctx, e, e.Timeout, func() (bool, error) {
		var sg guestctl.Object
		found, err := guestctl.Get(ctx, a, &sg, "get", "seedgenerators", "seedimage", "--ignore-not-found", "--request-timeout=10s")
		if err != nil { // the API is down: LCA is generating the seed on the node
			line, _ := guestctl.SSHOut(ctx, a, journalCmd...)
			ps.Update("cluster stopped: " + line)
			return false, nil
		}
		if !found {
			ps.Update("waiting for LCA to restore the SeedGenerator")
			return false, nil
		}
		p, msg := phaseOf(sg)
		ps.Update(msg)
		if p == sgFailed {
			failure = fmt.Errorf("seed generation failed: %s", msg)
		}
		return p != sgRunning, nil
	})
	switch {
	case failure != nil:
		return failure
	case errors.Is(err, guestctl.ErrTimeout):
		return fmt.Errorf("seed generation not finished after %s; it may still finish (`vsno oc <name> -- get seedgenerator seedimage`)", e.Timeout)
	}
	return err
}

// finish removes the SeedGenerator and its Secret, records the image on the
// vSNO's VM and stops it if asked. Failures here don't fail the seed.
func finish(ctx context.Context, e Env, a Access, o CreateOptions, res *Result) {
	ps := e.Progress.Start("Cleaning up")
	var warn []string
	if err := guestctl.Oc(ctx, a, "", "delete", "seedgenerators", "seedimage", "--ignore-not-found"); err != nil {
		warn = append(warn, err.Error())
	}
	if err := guestctl.Oc(ctx, a, "", "delete", "secret", "seedgen", "-n", LCANamespace, "--ignore-not-found"); err != nil {
		warn = append(warn, err.Error())
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, AnnSeedImage, res.Image)
	if _, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(o.Namespace).Patch(ctx, o.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		warn = append(warn, "recording the seed on the VM: "+err.Error())
	}
	if o.Stop {
		if err := guest.SetRunning(ctx, e.Env, o.Namespace, o.Name, false); err != nil {
			warn = append(warn, "stopping: "+err.Error())
		} else {
			res.Stopped = true
		}
	}
	if len(warn) > 0 {
		ps.Warn(strings.Join(warn, "; "))
		return
	}
	ps.Done("removed the SeedGenerator")
}
