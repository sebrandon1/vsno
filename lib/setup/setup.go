// Package setup prepares a host cluster for vSNOs: OpenShift Virtualization,
// LVM Storage, and the support services (DNS, builder, S3, registry) in the
// vsno namespace. Every step reuses what already exists, so reruns are no-ops.
package setup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/apply"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/state"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// StageName is the name setup records in the state ConfigMap.
const StageName = "setup"

// Env is the cluster access and UI setup needs.
type Env struct {
	Kube    kubernetes.Interface
	Dyn     dynamic.Interface
	Applier apply.Applier
	// Precheck runs the precheck, limited to node when it is set.
	Precheck func(ctx context.Context, node string) *precheck.Report
	// Confirm asks the user to type answer to approve question. Nil never approves.
	Confirm  func(question, answer string) bool
	Progress precheck.Progress // nil = silent
	Now      func() time.Time  // nil = time.Now
	Poll     time.Duration     // wait interval; 0 = 5s
	Timeout  time.Duration     // limit for each wait; 0 = 20m
}

// Options are the user's choices.
type Options struct {
	Namespace string
	// Settings from --config and flags. Empty fields come from the precheck.
	Settings precheck.Suggested
	Yes      bool // approve taking over the LVMS disk
	DryRun   bool // render and report, change nothing
	Images   manifests.Images
}

// Outcome says what a step did.
type Outcome string

const (
	// Reused means the step found the component already installed and left it alone.
	Reused Outcome = "reused"
	// Applied means vsno applied its manifests (which may all be unchanged).
	Applied Outcome = "applied"
	// Planned is Applied in a dry run.
	Planned Outcome = "planned"
)

// Step is one line of the setup report.
type Step struct {
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"`
	Detail  string  `json:"detail"`
}

// Result is the setup report.
type Result struct {
	Namespace string             `json:"namespace"`
	Settings  precheck.Suggested `json:"settings"`
	Warnings  []string           `json:"warnings,omitempty"`
	Steps     []Step             `json:"steps"`
	Changes   []apply.Change     `json:"changes"`
	// Registry is the in-cluster registry endpoint guests use (ClusterIP:port).
	Registry string `json:"registry,omitempty"`
	// Objects are the rendered objects of a dry run, with Secret values redacted.
	Objects []*unstructured.Unstructured `json:"-"`
}

// Run performs setup. On error the Result holds the steps completed so far.
func Run(ctx context.Context, e Env, o Options) (*Result, error) {
	e.defaults()
	res := &Result{Namespace: o.Namespace}
	rec := &recorder{}
	if o.DryRun {
		e.Applier = rec
	}
	s, warnings, err := resolve(e.Precheck(ctx, o.Settings.Node), o.Settings)
	res.Settings, res.Warnings = s, warnings
	if err != nil {
		return res, err
	}
	r := &runner{Env: e, o: o, res: res, cfg: manifests.Config{
		Namespace:    o.Namespace,
		Node:         s.Node,
		DiskByID:     s.Disk,
		HostDNS:      s.HostDNS,
		CNVChannel:   s.CNVChannel,
		LVMSChannel:  s.LVMSChannel,
		BaseDomain:   manifests.DefaultBaseDomain,
		Guests:       []string{"seed", "target"},
		StorageClass: manifests.DefaultStorageClass,
		Images:       o.Images,
		S3Bucket:     manifests.DefaultS3Bucket,
	}, st: &state.State{Versions: map[string]string{}, Services: map[string]string{}}}

	steps := []struct {
		title string
		run   func(context.Context, precheck.ProgressStep) (Step, error)
	}{
		{"OpenShift Virtualization operator", r.operator(cnv)},
		{"LVM Storage operator", r.operator(lvms)},
		{"HyperConverged", r.hyperConverged},
		{"LVMS storage", r.storage},
		{"Namespace " + o.Namespace, r.namespace},
		{"Support services", r.support},
	}
	for _, st := range steps {
		ps := e.Progress.Start(st.title)
		step, err := st.run(ctx, ps)
		if err != nil {
			ps.Fail(err.Error())
			_ = r.saveState(ctx, state.Failed, st.title+": "+err.Error())
			return res, err
		}
		ps.Done(string(step.Outcome) + ": " + step.Detail)
		res.Steps = append(res.Steps, step)
	}
	res.Objects = rec.objs
	if err := r.saveState(ctx, state.Done, summary(res)); err != nil {
		return res, err
	}
	return res, nil
}

func (e *Env) defaults() {
	if e.Progress == nil {
		e.Progress = precheck.NoProgress()
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Poll == 0 {
		e.Poll = 5 * time.Second
	}
	if e.Timeout == 0 {
		e.Timeout = 20 * time.Minute
	}
}

// runner carries the state shared by the steps of one Run.
type runner struct {
	Env
	o     Options
	res   *Result
	cfg   manifests.Config
	st    *state.State
	nsSet bool // the namespace exists, so state can be saved
}

// applyTemplate renders a template, keeps the objects keep selects (nil = all),
// applies them and returns a "n created, n updated, n unchanged" detail.
func (r *runner) applyTemplate(ctx context.Context, name string, keep func(*unstructured.Unstructured) bool) (string, error) {
	objs, err := manifests.Render(name, r.cfg)
	if err != nil {
		return "", err
	}
	if keep != nil {
		kept := objs[:0]
		for _, o := range objs {
			if keep(o) {
				kept = append(kept, o)
			}
		}
		objs = kept
	}
	ch, err := r.Applier.Apply(ctx, objs)
	r.res.Changes = append(r.res.Changes, ch...)
	if err != nil {
		return "", err
	}
	return r.detail(ch), nil
}

// detail summarizes changes; a dry run only knows how many objects it would apply.
func (r *runner) detail(ch []apply.Change) string {
	if r.o.DryRun {
		return fmt.Sprintf("%d object(s)", len(ch))
	}
	return countDetail(ch)
}

func countDetail(ch []apply.Change) string {
	return fmt.Sprintf("%d created, %d updated, %d unchanged",
		apply.Count(ch, apply.Created), apply.Count(ch, apply.Updated), apply.Count(ch, apply.Unchanged))
}

// outcome is Planned in a dry run, Applied otherwise.
func (r *runner) outcome() Outcome {
	if r.o.DryRun {
		return Planned
	}
	return Applied
}

// saveState records the stage. It does nothing in a dry run or before the
// namespace exists. A failure is reported only when the stage succeeded.
func (r *runner) saveState(ctx context.Context, status state.StageStatus, detail string) error {
	if r.o.DryRun || !r.nsSet {
		return nil
	}
	st, _, err := state.Load(ctx, r.Kube, r.o.Namespace)
	if err != nil {
		st = &state.State{}
	}
	if status == state.Done {
		st.Node, st.Disk, st.HostDNS = r.cfg.Node, r.cfg.DiskByID, r.cfg.HostDNS
		st.Services, st.Versions = r.st.Services, r.st.Versions
	}
	st.Record(StageName, status, detail, r.Now())
	return state.Save(ctx, r.Kube, r.o.Namespace, st)
}

func summary(res *Result) string {
	parts := make([]string, 0, len(res.Steps))
	for _, s := range res.Steps {
		parts = append(parts, s.Name+" "+string(s.Outcome))
	}
	return strings.Join(parts, ", ")
}

// recorder is the dry-run Applier: it keeps the objects and changes nothing.
type recorder struct{ objs []*unstructured.Unstructured }

func (d *recorder) Apply(_ context.Context, objs []*unstructured.Unstructured) ([]apply.Change, error) {
	ch := make([]apply.Change, 0, len(objs))
	for _, o := range objs {
		d.objs = append(d.objs, redact(o))
		ch = append(ch, apply.Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), Action: apply.Created})
	}
	return ch, nil
}

// redact returns a copy of o with Secret values replaced.
func redact(o *unstructured.Unstructured) *unstructured.Unstructured {
	if o.GetKind() != "Secret" {
		return o
	}
	c := o.DeepCopy()
	for _, f := range []string{"stringData", "data"} {
		m, _, _ := unstructured.NestedMap(c.Object, f)
		for k := range m {
			m[k] = "<redacted>"
		}
		if m != nil {
			_ = unstructured.SetNestedMap(c.Object, m, f)
		}
	}
	return c
}
