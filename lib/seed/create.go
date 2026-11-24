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
	// operatorWait is how long an operator's CSV may take to succeed.
	operatorWait = 10 * time.Minute
	trustWait    = 10 * time.Minute
)

// Create makes a seed image from vSNO o.Name: it checks the cluster is
// stable and readies the vSNO — the Lifecycle Agent and OADP from its
// catalog, and trust in the environment's registry, which is what
// `vsno seed precheck --install` sets up — then runs LCA's seed generation
// and follows it until the image is pushed and the cluster is back.
func Create(ctx context.Context, e Env, o CreateOptions) (*Result, error) {
	e.Defaults(o.Namespace)
	res := &Result{Name: o.Name, Image: o.Image}
	// Before the precheck: it spends minutes on the vSNO, and a generation that
	// is already running is one `oc get seedgenerator` away.
	if err := checkSeedGen(ctx, e, o.Namespace, o.Name); err != nil {
		return res, err
	}
	var reg Registry
	if err := guestctl.Step(e.Progress, "Checking "+o.Name, func(precheck.ProgressStep) (string, error) {
		// Install is what makes this command do what its help says: a vSNO
		// without the Lifecycle Agent, OADP or the registry's trust gets them
		// here, rather than failing and sending the user to
		// `seed precheck --install`.
		rep, err := Precheck(ctx, e, PrecheckOptions{Namespace: o.Namespace, Name: o.Name, Image: o.Image, Settle: true, Install: true})
		if err != nil {
			return "", err
		}
		res.Version, res.Image, res.LCA, reg = rep.Version, rep.Image, rep.LCA, rep.Registry
		return res.Version + ", " + res.Image, failure(rep)
	}); err != nil {
		return res, err
	}
	a := e.Access(o.Name)
	auth, err := seedAuth(ctx, e.Kube, reg, res.Image)
	if err != nil {
		return res, err
	}
	if err := guestctl.Step(e.Progress, "Generating the seed image", func(ps precheck.ProgressStep) (string, error) {
		start := e.Now()
		setSeeding(ctx, e, o.Namespace, o.Name, start)
		err := generate(ctx, e, a, res.Image, auth, ps)
		// An interrupted run leaves LCA generating on the node, with the guest's
		// services stopped: the mark has to stay until the SeedGenerator says the
		// generation ended, or `vsno list` reads the silence as a fault.
		if seedGenEnded(ctx, a) {
			setSeeding(context.WithoutCancel(ctx), e, o.Namespace, o.Name, time.Time{})
		}
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

// LCADeployment is LCA's controller, as both its catalog bundle and its
// source's config/default name it.
const LCADeployment = "lifecycle-agent-controller-manager"

// SourceLCA reports whether the vSNO runs LCA deployed from a ref rather than
// by OLM, and its controller image: OLM labels what it deploys with olm.owner.
func SourceLCA(ctx context.Context, a Access) (image string, fromSource bool, err error) {
	var d guestctl.Deployment
	found, err := guestctl.Get(ctx, a, &d, "get", "deployments", LCADeployment, "-n", LCANamespace, "--ignore-not-found")
	if err != nil || !found || d.Metadata.Labels["olm.owner"] != "" {
		return "", false, err
	}
	image = d.Image("manager")
	return image, image != "", nil
}

// InstalledLCA names the LCA a vSNO runs by the image of its controller, so
// the two ways of installing the same operator compare equal: OLM sets the
// image on the Deployment too. It falls back to the CSV when the Deployment
// can't be read, and is "" when no LCA runs.
func InstalledLCA(ctx context.Context, a Access) (string, error) {
	var d guestctl.Deployment
	found, err := guestctl.Get(ctx, a, &d, "get", "deployments", LCADeployment, "-n", LCANamespace, "--ignore-not-found")
	if err == nil && found {
		if image := d.Image("manager"); image != "" {
			return image, nil
		}
	}
	csv, _, err := LCA.CSV(ctx, a)
	return csv, err
}

// ensureLCA installs LCA from the vSNO's own catalog unless it's there. LCA
// deployed from a ref (vsno prepare --lca-ref) is kept: subscribing would
// replace it.
func ensureLCA(ctx context.Context, e Env, a Access, ps precheck.ProgressStep) (string, error) {
	if image, fromSource, err := SourceLCA(ctx, a); err != nil || fromSource {
		return image, err
	}
	return LCA.Ensure(ctx, a, e.Clock(), operatorWait, ps)
}

// imageConfig is the part of the cluster image configuration vsno reads.
type imageConfig struct {
	Spec struct {
		AdditionalTrustedCA struct {
			Name string `json:"name"`
		} `json:"additionalTrustedCA"`
	} `json:"spec"`
}

// Name is the ConfigMap the vSNO trusts CAs from; "" when it trusts none.
func (c imageConfig) Name() string { return c.Spec.AdditionalTrustedCA.Name }

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

// seedGenQuery is the SeedGenerator read that answers "is one running?", asked
// ahead of the precheck with a timeout short enough to refuse in seconds.
const seedGenQuery = "--request-timeout=15s"

// seedGen reads the SeedGenerator LCA runs: found is false when there is none,
// and err when the guest's API doesn't answer.
func seedGen(ctx context.Context, a Access) (sg guestctl.Object, found bool, err error) {
	found, err = guestctl.Get(ctx, a, &sg, "get", "seedgenerators", "seedimage", "--ignore-not-found", seedGenQuery)
	return sg, found, err
}

// checkSeedGen refuses a second generation before the precheck spends minutes
// on the vSNO. LCA's SeedGenerator is the answer while the guest's API is up;
// while it is down — which a generation causes — the mark vsno left on the VM
// is. A generation that ended while nothing followed it leaves a stale mark,
// which is cleared.
func checkSeedGen(ctx context.Context, e Env, ns, name string) error {
	if sg, found, err := seedGen(ctx, e.Access(name)); err == nil && found {
		if p, msg := phaseOf(sg); p == sgRunning {
			return fmt.Errorf("a seed generation is already running on the vSNO: %s", msg)
		}
		setSeeding(ctx, e, ns, name, time.Time{}) // it ended while nothing was following it
		return nil
	}
	// No SeedGenerator to read, or no answer at all: the mark is what is left.
	at, ok := seedingMark(ctx, e, ns, name)
	if !ok {
		return nil
	}
	return fmt.Errorf("vsno marked a seed generation on the vSNO as started %s ago, and its SeedGenerator can't be read: "+
		"LCA is still generating. `vsno list` follows it; run this again when it ends", e.Now().Sub(at).Round(time.Second))
}

// SeedGen is a seed generation under way on a vSNO.
type SeedGen struct {
	Message string        // LCA's own message, when its SeedGenerator can be read
	Age     time.Duration // since vsno marked it, when the mark is what said so
}

// String names the generation: LCA's message, or how long the mark has stood.
func (g SeedGen) String() string {
	if g.Message != "" {
		return g.Message
	}
	return "started " + g.Age.Round(time.Second).String() + " ago"
}

// RunningSeedGen reports the seed generation under way on the vSNO, or nil.
// LCA's SeedGenerator says so while the guest's API is up; while it is down,
// which a generation causes, the mark vsno left on the VM does.
func RunningSeedGen(ctx context.Context, e Env, ns, name string) *SeedGen {
	if sg, found, err := seedGen(ctx, e.Access(name)); err == nil && found {
		if p, msg := phaseOf(sg); p == sgRunning {
			return &SeedGen{Message: msg}
		}
		return nil
	}
	if at, ok := seedingMark(ctx, e, ns, name); ok {
		return &SeedGen{Age: e.Now().Sub(at)}
	}
	return nil
}

// orchDir is where LCA keeps a seed generation's state on the node.
const orchDir = "/var/lib/lca/ibu-seedgen-orch"

// SeedGenInterrupted reports the aftermath of a vSNO stopped while LCA was
// making a seed image: the generation's directory is still on the node and
// crio and kubelet are disabled, so the guest comes up without an API.
func SeedGenInterrupted(ctx context.Context, a Access) (bool, error) {
	out, err := guestctl.SSHOut(ctx, a, "test", "-d", orchDir, "&&", "echo", "orch", ";", "systemctl", "is-enabled", "kubelet", "crio")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "orch") && strings.Contains(out, "disabled"), nil
}

// seedingMark is when vsno marked the vSNO as generating, if the mark is recent
// enough to mean the generation is running now.
func seedingMark(ctx context.Context, e Env, ns, name string) (time.Time, bool) {
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return time.Time{}, false
	}
	return guest.SeedingSince(vm.GetAnnotations()[guest.AnnSeeding], e.Now())
}

// seedGenEnded reports whether the SeedGenerator LCA runs is gone or finished.
// It reads with the run's cancellation dropped: the mark must be cleared only
// when the generation really ended, and a canceled context reaches nothing.
func seedGenEnded(ctx context.Context, a Access) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	sg, found, err := seedGen(ctx, a)
	if err != nil {
		return false // can't tell: keep the mark, which list treats as recent for an hour
	}
	if !found {
		return true
	}
	p, _ := phaseOf(sg)
	return p != sgRunning
}

// setSeeding marks the vSNO's VM while seed generation runs: LCA stops the
// guest's services, so `vsno list` reads the mark to say what the silence is.
// A zero time removes it. Best effort: the mark only changes what list says.
func setSeeding(ctx context.Context, e Env, ns, name string, at time.Time) {
	val := "null"
	if !at.IsZero() {
		val = `"` + at.UTC().Format(time.RFC3339) + `"`
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%s}}}`, guest.AnnSeeding, val)
	_, _ = e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
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
