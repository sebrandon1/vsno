package precheck

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Options narrows what Run does.
type Options struct {
	// Node limits the hardware probe to one node (empty = all candidate workers).
	Node string
}

// Run executes every check and fills in Suggested from the best eligible node.
func Run(ctx context.Context, e *Env, req Requirements, opts Options) *Report {
	r := &Report{}
	step := e.progress().Start("Checking cluster configuration")
	if !e.checkClusterAdmin(ctx, r) {
		step.Fail("no cluster-admin access")
		return r // nothing else will work
	}
	e.checkHostVersion(ctx, r)
	e.checkNetworks(ctx, r, req)
	e.checkHostDNS(ctx, r)
	e.checkCatalog(ctx, r)
	storageReady := e.checkExisting(ctx, r)
	e.checkZTP(ctx, r)
	e.checkSeedRepo(r, req)
	step.Done(r.Suggested.HostVersion)
	e.checkNodes(ctx, r, req, opts, storageReady)
	return r
}

func (e *Env) checkNodes(ctx context.Context, r *Report, req Requirements, opts Options, storageReady bool) {
	workers, ok := e.listWorkers(ctx, r, opts)
	if !ok {
		return
	}
	if e.Prober == nil {
		r.add(Result{Check: "Hardware probe", Status: Warn, Detail: "skipped (--no-probe); cannot confirm KVM, CPUs or disks"})
		return
	}
	verdicts := e.evaluateWorkers(ctx, r, workers, req, storageReady)
	summarize(r, verdicts)
}

// listWorkers returns the candidate workers (optionally just opts.Node) and records the result.
func (e *Env) listWorkers(ctx context.Context, r *Report, opts Options) ([]corev1.Node, bool) {
	nodes, err := e.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		r.add(Result{Check: "Worker nodes", Status: Fail, Detail: err.Error()})
		return nil, false
	}
	workers := candidateWorkers(nodes.Items)
	if opts.Node != "" {
		only := workers[:0]
		for _, n := range workers {
			if n.Name == opts.Node {
				only = append(only, n)
			}
		}
		workers = only
	}
	if len(workers) == 0 {
		msg := "no Ready, schedulable worker nodes"
		if opts.Node != "" {
			msg = fmt.Sprintf("node %q is not a Ready, schedulable worker", opts.Node)
		}
		r.add(Result{Check: "Worker nodes", Status: Fail, Detail: msg})
		return nil, false
	}
	detail := fmt.Sprintf("%d candidate(s)", len(workers))
	if e.Prober == nil { // no per-node blocks follow, so name them here
		detail += ": " + strings.Join(nodeNames(workers), ", ")
	}
	r.add(Result{Check: "Worker nodes", Status: Info, Detail: detail})
	return workers, true
}

func nodeNames(nodes []corev1.Node) []string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	return names
}

// evaluateWorkers probes the workers and evaluates each one that answered.
func (e *Env) evaluateWorkers(ctx context.Context, r *Report, workers []corev1.Node, req Requirements, storageReady bool) []NodeVerdict {
	names := nodeNames(workers)
	step := e.progress().Start(fmt.Sprintf("Probing %d worker(s)", len(names)))
	step.SetProgress(0, len(names))
	var mu sync.Mutex
	finished := 0
	hw, probeErrs := e.Prober.Probe(ctx, names, func(node string, err error) {
		mu.Lock()
		defer mu.Unlock()
		finished++
		step.SetProgress(finished, len(names))
		mark := "✓"
		if err != nil {
			mark = "✗"
		}
		step.Update(node + " " + mark)
	})
	if len(probeErrs) > 0 {
		step.Warn(fmt.Sprintf("%d of %d probes failed", len(probeErrs), len(names)))
	} else {
		step.Done(fmt.Sprintf("%d probed", len(names)))
	}
	var verdicts []NodeVerdict
	for _, n := range workers {
		if perr := probeErrs[n.Name]; perr != nil {
			r.add(Result{Node: n.Name, Check: "probe", Status: Warn, Detail: perr.Error()})
			continue
		}
		h, ok := hw[n.Name]
		if !ok {
			continue
		}
		free, held, ferr := e.freeMemory(ctx, n)
		if ferr != nil {
			r.add(Result{Node: n.Name, Check: "memory", Status: Warn, Detail: ferr.Error()})
			continue
		}
		// LVMS storage only helps on the node(s) where TopoLVM actually runs.
		v := EvaluateNode(n.Name, h, free+held, req, storageReady && e.hasTopoLVM(ctx, n.Name))
		if held > 0 {
			v.insertAfter("memory", Result{Node: n.Name, Check: "memory", Status: Info,
				Detail: fmt.Sprintf("%d GiB held by the existing %s environment is counted as available", held>>30, e.Namespace)})
		}
		verdicts = append(verdicts, v)
	}
	return verdicts
}

// summarize records per-node results and the overall verdict, and fills Suggested.
func summarize(r *Report, verdicts []NodeVerdict) {
	// Eligible nodes first, best score first, so the summary reads naturally.
	sort.SliceStable(verdicts, func(i, j int) bool {
		if verdicts[i].Eligible != verdicts[j].Eligible {
			return verdicts[i].Eligible
		}
		return verdicts[i].Score > verdicts[j].Score
	})
	eligible := 0
	for _, v := range verdicts {
		if v.Eligible {
			eligible++
		}
	}
	for _, v := range verdicts {
		if !v.Eligible && eligible > 0 {
			demoteFailures(v.Results)
		}
		for _, res := range v.Results {
			r.add(res)
		}
		r.Nodes = append(r.Nodes, NodeSummary{Name: v.Node, Eligible: v.Eligible})
	}
	if eligible == 0 {
		r.add(Result{Check: "Eligible node", Status: Fail, Detail: "no worker can host vSNO VMs",
			Hint: "See the per-node failures above. You need a worker with /dev/kvm, enough CPU and memory, and an empty disk."})
		return
	}
	best := verdicts[0]
	r.Suggested.Node = best.Node
	if best.Disk != nil {
		r.Suggested.Disk = best.Disk.PreferredByID()
		r.Suggested.DiskSizeGB = best.Disk.SizeGB()
	}
	r.add(Result{Check: "Eligible node", Status: Pass, Detail: fmt.Sprintf("%s (of %d eligible)", best.Node, eligible)})
}

// demoteFailures turns a node's failures into context: another node will be used.
func demoteFailures(results []Result) {
	for i := range results {
		if results[i].Status == Fail {
			results[i].Status = Info
			results[i].Detail = "not usable: " + results[i].Detail
			results[i].Hint = ""
		}
	}
}
