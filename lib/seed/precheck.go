package seed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/precheck"
)

// The seed precheck's report sections.
const (
	GroupVSNO     precheck.Group = "vsno"     // the vSNO and its cluster
	GroupRegistry precheck.Group = "registry" // the environment's registry, and the vSNO trusting it
	GroupLCA      precheck.Group = "lca"      // the Lifecycle Agent, which generates the seed
	GroupTarget   precheck.Group = "target"   // where the seed image goes
)

// The checks' names in the report.
const (
	checkLCA   = "Lifecycle Agent"
	checkTrust = "Registry trust"
	checkSeed  = "Seed target"
	checkRoom  = "Registry space"
)

// How much free space in the environment's registry a seed generation is
// comfortable with. The image a seed run pushes is around 1.5 GiB on this lab,
// but it is built from scratch each time, so the floor is generous.
const (
	seedFloorFail = 2 << 30
	seedFloorWarn = 5 << 30
)

// RoomFail and RoomWarn are the free-space floors a seed generation needs in
// the environment's registry, for callers outside this package (vsno doctor).
func RoomFail() int64 { return seedFloorFail }
func RoomWarn() int64 { return seedFloorWarn }

// PrecheckOptions are the user's choices for vsno seed precheck.
type PrecheckOptions struct {
	Namespace string
	Name      string
	Image     string // "" = Ref in the environment's registry
	// Install sets up what it can, the same way `vsno prepare` does: the
	// Lifecycle Agent and OADP from the vSNO's own catalog, and trust in the
	// environment's registry.
	Install bool
	// Settle waits for the cluster to settle instead of failing on it that it
	// hasn't. Create uses it: the checks are the same, only patient.
	Settle bool
}

// PrecheckReport is what a seed generation needs, and got set up.
type PrecheckReport struct {
	VSNO    string `json:"vsno"`
	Version string `json:"version,omitempty"`
	Image   string `json:"image,omitempty"`
	LCA     string `json:"lca,omitempty"`
	// Results are the checks, as `vsno precheck` reports them.
	Results []precheck.Result `json:"results"`
	// Ready are what --install set up: the operators it installed, and the
	// registry trust it added. What was already there is in the checks.
	Ready []string `json:"ready,omitempty"`
	// Warnings are setup steps --install couldn't finish; the checks say why.
	Warnings []string `json:"warnings,omitempty"`
	// Registry is the environment's registry, for later steps.
	Registry Registry `json:"-"`
}

// Failed counts the failing checks.
func (r *PrecheckReport) Failed() int {
	n := 0
	for _, res := range r.Results {
		if res.Status == precheck.Fail {
			n++
		}
	}
	return n
}

// Precheck reports what `seed create` needs from vSNO o.Name, and with Install
// sets up the parts it can. Failing checks are in the report, not an error:
// only a failure to list the vSNOs is.
func Precheck(ctx context.Context, e Env, o PrecheckOptions) (*PrecheckReport, error) {
	e.Defaults(o.Namespace)
	rep := &PrecheckReport{VSNO: o.Name}
	g, err := find(ctx, e, o)
	if err != nil {
		return rep, err
	}
	if g == nil {
		rep.Results = []precheck.Result{{Group: GroupVSNO, Check: "vSNO " + o.Name, Status: precheck.Fail,
			Detail: "not found in namespace " + o.Namespace, Hint: "see `vsno list`"}}
		return rep, nil
	}
	rep.Version = g.ShownVersion()
	rep.Registry, err = LoadRegistry(ctx, e.Kube, o.Namespace)
	switch {
	case o.Image != "":
		rep.Image = o.Image
	case rep.Version != "":
		rep.Image = Ref(rep.Registry, g.Name, rep.Version)
	}
	in := &inspector{Env: e, o: o, g: g, a: e.Access(g.Name), reg: rep.Registry, regErr: err, rep: rep}
	in.run(ctx)
	if o.Install {
		install(ctx, e, in)
		rep.Results = nil
		in.run(ctx)
	}
	return rep, nil
}

// find returns the vSNO o.Name, or nil when there is none.
func find(ctx context.Context, e Env, o PrecheckOptions) (*guest.Guest, error) {
	all, err := guest.List(ctx, e.Env, o.Namespace)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(all, func(g guest.Guest) bool { return g.Name == o.Name })
	if i < 0 {
		return nil, nil
	}
	return &all[i], nil
}

// inspector fills a report's checks for one vSNO.
type inspector struct {
	Env
	o      PrecheckOptions
	g      *guest.Guest
	a      Access
	reg    Registry
	regErr error
	rep    *PrecheckReport
}

func (in *inspector) run(ctx context.Context) {
	in.vsno()
	in.registry()
	in.api(ctx)
	in.lifecycleAgent(ctx)
	in.trust(ctx)
	in.target(ctx)
	in.room(ctx)
}

func (in *inspector) row(group precheck.Group, st precheck.Status, check, detail, hint string) {
	in.rep.Results = append(in.rep.Results, precheck.Result{Check: check, Status: st, Detail: detail, Hint: hint, Group: group})
}

// vsno reports the vSNO's state, version and size. A seed needs a finished
// install, so anything else fails with the state in the detail.
func (in *inspector) vsno() {
	g := in.g
	v := g.ShownVersion()
	if v == "" {
		v = "unknown"
	}
	detail := fmt.Sprintf("%s, OpenShift %s, %d vCPU / %d GiB / %d GiB", g.State, v, g.Cores, g.MemoryGiB, g.DiskGiB)
	if g.Detail != "" {
		detail += " (" + g.Detail + ")"
	}
	st, hint := precheck.Pass, ""
	if g.State != guest.Ready {
		st = precheck.Fail
		hint = "a seed needs a finished install; `vsno start " + g.Name + "` brings a stopped vSNO back"
	}
	in.row(GroupVSNO, st, "vSNO "+g.Name, detail, hint)
}

// registry reports whether the environment's registry, which holds the seed
// image, is usable.
func (in *inspector) registry() {
	if in.regErr != nil {
		in.row(GroupRegistry, precheck.Fail, "Registry", in.regErr.Error(), "rerun `vsno setup -n "+in.o.Namespace+"`")
		return
	}
	in.row(GroupRegistry, precheck.Pass, "Registry", in.reg.Host+", with its CA and credentials", "")
}

// api reports whether the vSNO answers and its cluster is settled. With
// Settle, an unsettled cluster is waited for.
func (in *inspector) api(ctx context.Context) {
	var cos, mcps guestctl.ObjectList
	if _, err := guestctl.Get(ctx, in.a, &cos, "get", "clusteroperators"); err != nil {
		in.row(precheck.GroupCluster, precheck.Fail, "vSNO API", err.Error(), "is the vSNO running? `vsno list`")
		return
	}
	in.row(precheck.GroupCluster, precheck.Pass, "vSNO API", "answers", "")
	if _, err := guestctl.Get(ctx, in.a, &mcps, "get", "machineconfigpools"); err != nil {
		in.row(precheck.GroupCluster, precheck.Fail, "Cluster settled", err.Error(), "")
		return
	}
	why := unstable(cos, mcps)
	if len(why) > 0 && in.o.Settle {
		err := guestctl.Step(in.Progress, "Waiting for a stable cluster", func(ps precheck.ProgressStep) (string, error) {
			return "", waitStable(ctx, in.Env, in.a, ps)
		})
		if err != nil {
			why = []string{err.Error()}
		} else {
			why = nil
		}
	}
	if len(why) > 0 {
		in.row(precheck.GroupCluster, precheck.Fail, "Cluster settled", strings.Join(why, ", "),
			"seed create waits up to "+stableWait.String()+" for this to clear")
		return
	}
	in.row(precheck.GroupCluster, precheck.Pass, "Cluster settled", "every ClusterOperator Available and every MachineConfigPool Updated", "")
}

// lifecycleAgent reports whether LCA is installed, deployed from a ref
// (vsno prepare --lca-ref) or by OLM, or installable from the vSNO's own
// catalog.
func (in *inspector) lifecycleAgent(ctx context.Context) {
	if image, fromSource, err := SourceLCA(ctx, in.a); err != nil {
		in.row(GroupLCA, precheck.Fail, checkLCA, err.Error(), "")
		return
	} else if fromSource {
		in.rep.LCA = image
		in.row(GroupLCA, precheck.Pass, checkLCA, "deployed from source: "+image, "")
		return
	}
	name, ok, err := LCA.CSV(ctx, in.a)
	switch {
	case err != nil:
		in.row(GroupLCA, precheck.Fail, checkLCA, err.Error(), "")
	case name != "" && ok:
		in.rep.LCA = name
		in.row(GroupLCA, precheck.Pass, checkLCA, name+" Succeeded", "")
	case name != "":
		in.rep.LCA = name
		in.row(GroupLCA, precheck.Warn, checkLCA, name+" is not Succeeded yet", "wait for it, or `vsno oc "+in.g.Name+" -- get csv -n "+LCANamespace+"`")
	default:
		pm, found, err := LCA.Catalog(ctx, in.a)
		switch {
		case err != nil:
			in.row(GroupLCA, precheck.Fail, checkLCA, err.Error(), "")
		case !found || pm.Status.DefaultChannel == "":
			in.row(GroupLCA, precheck.Fail, checkLCA, "not installed, and the vSNO's operator catalog has no lifecycle-agent package",
				"expected for a nightly before its release; use a released vSNO")
		default:
			in.row(GroupLCA, precheck.Fail, checkLCA, "not installed",
				"--install subscribes to "+pm.Status.CatalogSource+"/"+pm.Status.DefaultChannel)
		}
	}
}

// trust reports whether the vSNO trusts the registry's CA, which the seed
// image's push and pull need.
func (in *inspector) trust(ctx context.Context) {
	switch {
	case in.regErr != nil:
		return // the Registry row says why
	case in.rep.Image != "" && imageHost(in.rep.Image) != in.reg.Host:
		in.row(GroupRegistry, precheck.Info, checkTrust, "not needed: the seed goes to "+imageHost(in.rep.Image), "")
		return
	}
	var img imageConfig
	if _, err := guestctl.Get(ctx, in.a, &img, "get", "image.config.openshift.io", "cluster"); err != nil {
		in.row(GroupRegistry, precheck.Fail, checkTrust, err.Error(), "")
		return
	}
	file := certFile(in.reg)
	switch name := img.Spec.AdditionalTrustedCA.Name; name {
	case trustConfigMap:
		if guestctl.SSH(ctx, in.a, "test", "-f", file) != nil {
			in.row(GroupRegistry, precheck.Warn, checkTrust, trustConfigMap+" is configured, but the node doesn't have "+file+" yet",
				"wait for the node's MachineConfig to finish")
			return
		}
		in.row(GroupRegistry, precheck.Pass, checkTrust, trustConfigMap+", and the node has "+file, "")
	case "":
		in.row(GroupRegistry, precheck.Fail, checkTrust, "image.config/cluster has no additionalTrustedCA",
			"--install adds ConfigMap "+trustConfigMap+" and names it in image.config/cluster")
	default:
		in.row(GroupRegistry, precheck.Fail, checkTrust, "the vSNO trusts CA ConfigMap openshift-config/"+name,
			"add key "+in.reg.TrustKey()+" with the registry CA to ConfigMap openshift-config/"+name)
	}
}

// target reports the seed image's reference and that vsno has credentials to
// push it.
func (in *inspector) target(ctx context.Context) {
	if in.rep.Image == "" {
		in.row(GroupTarget, precheck.Fail, checkSeed, "the vSNO's version is unknown, so there is no default image", "pass --image <ref>")
		return
	}
	if imageHost(in.rep.Image) == in.reg.Host && in.regErr != nil {
		return // the Registry row says why
	}
	if _, err := seedAuth(ctx, in.Kube, in.reg, in.rep.Image); err != nil {
		in.row(GroupTarget, precheck.Fail, checkSeed, in.rep.Image, err.Error())
		return
	}
	in.row(GroupTarget, precheck.Pass, checkSeed, in.rep.Image+", and the credentials to push it", "")
}

// room reports whether the environment's registry can hold the seed image. A
// seed generation pushes about 1.5 GiB; finding out after the vSNO's services
// have been stopped costs the whole run, so say it here.
func (in *inspector) room(ctx context.Context) {
	if in.Volume == nil || in.regErr != nil || in.rep.Image == "" || imageHost(in.rep.Image) != in.reg.Host {
		return // not this registry, or a caller that cannot read the volume
	}
	used, total, err := in.Volume(ctx, in.o.Namespace)
	if err != nil {
		in.row(GroupRegistry, precheck.Warn, checkRoom, "the registry's volume couldn't be read: "+err.Error(), "")
		return
	}
	free := total - used
	detail := fmt.Sprintf("%s free of %s", gib(max(0, free)), gib(total))
	switch {
	case free < seedFloorFail:
		in.row(GroupRegistry, precheck.Fail, checkRoom, detail+"; a seed image needs about "+gib(seedFloorFail),
			"`vsno cache prune` or `vsno cache gc` frees space")
	case free < seedFloorWarn:
		in.row(GroupRegistry, precheck.Warn, checkRoom, detail+"; a seed image needs about "+gib(seedFloorFail),
			"`vsno cache prune` and `vsno cache gc` are the ways to free space")
	default:
		in.row(GroupRegistry, precheck.Pass, checkRoom, detail, "")
	}
}

// gib renders a byte count the way the tables do.
func gib(n int64) string { return fmt.Sprintf("%.1fGi", float64(n)/(1<<30)) }

// install sets up what it can, before the checks are run again. It takes the
// same steps `vsno prepare` does, so a vSNO readied here and one readied there
// end up the same. Only what was missing before is reported: the checks name
// everything that was already there.
func install(ctx context.Context, e Env, in *inspector) {
	wasLCA, wasTrust := in.passed(checkLCA), in.passed(checkTrust)
	name, ok, _ := OADP.CSV(ctx, in.a)
	wasOADP := ok && name != ""
	r, err := EnsureReady(ctx, e, in.a, PrepOptions{Registry: in.reg, NoTrust: !trustsRegistry(ctx, in)})
	// Report what is in place even when a later step failed.
	if r.LCA != "" && !wasLCA {
		in.rep.Ready = append(in.rep.Ready, "Lifecycle Agent "+r.LCA)
	}
	if r.OADP != "" && !wasOADP {
		in.rep.Ready = append(in.rep.Ready, "OADP "+r.OADP)
	}
	if r.Trusted && !wasTrust {
		in.rep.Ready = append(in.rep.Ready, "the vSNO trusting "+in.reg.Host)
	}
	if err != nil {
		in.rep.Warnings = append(in.rep.Warnings, err.Error())
	}
}

// passed is whether rep already passed check, before --install changed anything.
func (in *inspector) passed(check string) bool {
	for _, res := range in.rep.Results {
		if res.Check == check {
			return res.Status == precheck.Pass
		}
	}
	return false
}

// trustsRegistry is whether --install should set up trust: only when the seed
// image goes to the environment's registry, and only when the vSNO isn't
// already trusting CAs from a ConfigMap that isn't ours — that one is left
// alone, and trust() says which key to add to it.
func trustsRegistry(ctx context.Context, in *inspector) bool {
	if in.regErr != nil || in.rep.Image == "" || imageHost(in.rep.Image) != in.reg.Host {
		return false
	}
	var img imageConfig
	if _, err := guestctl.Get(ctx, in.a, &img, "get", "image.config.openshift.io", "cluster"); err != nil {
		return false // trust() reports the failed read
	}
	switch img.Name() {
	case trustConfigMap, "":
		return true
	}
	return false
}

// failure turns a report's failing checks into one error, with their hints.
func failure(r *PrecheckReport) error {
	var msgs []string
	for _, res := range r.Results {
		if res.Status != precheck.Fail {
			continue
		}
		m := res.Check + ": " + res.Detail
		if res.Hint != "" {
			m += "; " + res.Hint
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "; "))
}

// certFile is where the vSNO's nodes hold r's CA.
func certFile(r Registry) string {
	return "/etc/docker/certs.d/" + r.Host + "/ca.crt"
}
