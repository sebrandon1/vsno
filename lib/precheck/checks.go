package precheck

import (
	"context"
	"fmt"
	"sort"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// GVRs of the OpenShift / OLM / operator resources precheck reads.
var (
	GVRClusterVersion  = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "clusterversions"}
	GVRNetwork         = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "networks"}
	GVRPackageManifest = schema.GroupVersionResource{Group: "packages.operators.coreos.com", Version: "v1", Resource: "packagemanifests"}
	GVRCSV             = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "clusterserviceversions"}
	GVRHyperConverged  = schema.GroupVersionResource{Group: "hco.kubevirt.io", Version: "v1beta1", Resource: "hyperconvergeds"}
	GVRLVMCluster      = schema.GroupVersionResource{Group: "lvm.topolvm.io", Version: "v1alpha1", Resource: "lvmclusters"}
)

const (
	marketplaceNS = "openshift-marketplace"
	cnvNS         = "openshift-cnv"
	storageNS     = "openshift-storage"
	ztpAgentNS    = "open-cluster-management-agent"
	vsnoSC        = "lvms-vg-vsno"
)

// Prober collects NodeHardware from nodes. The real implementation runs a
// short-lived privileged pod per node; tests use fixtures.
// onDone, if non-nil, is called as each node finishes (from any goroutine).
type Prober interface {
	Probe(ctx context.Context, nodes []string, onDone func(node string, err error)) (map[string]NodeHardware, map[string]error)
}

// Env is everything the checks need.
type Env struct {
	Kube      kubernetes.Interface
	Dyn       dynamic.Interface
	Prober    Prober // nil skips the hardware probe
	Namespace string // vsno namespace, used to detect an existing environment
	AuthFiles []string
	Progress  Progress // nil = silent
}

func (e *Env) progress() Progress {
	if e.Progress == nil {
		return noopProgress{}
	}
	return e.Progress
}

func (e *Env) checkClusterAdmin(ctx context.Context, r *Report) bool {
	ssar := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authv1.ResourceAttributes{Verb: "*", Group: "*", Resource: "*"},
	}}
	res, err := e.Kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
	switch {
	case err != nil:
		r.add(Result{Check: "Cluster access", Status: Fail, Detail: err.Error(),
			Hint: "Check KUBECONFIG / --kubeconfig and that the API is reachable."})
		return false
	case !res.Status.Allowed:
		r.add(Result{Check: "Cluster access", Status: Fail, Detail: "current user is not cluster-admin",
			Hint: "vsno installs operators and runs privileged pods; log in as cluster-admin."})
		return false
	}
	r.add(Result{Check: "Cluster access", Status: Pass, Detail: "cluster-admin"})
	return true
}

func (e *Env) checkHostVersion(ctx context.Context, r *Report) {
	cv, err := e.Dyn.Resource(GVRClusterVersion).Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		r.add(Result{Check: "OpenShift version", Status: Fail, Detail: err.Error(),
			Hint: "vsno needs an OpenShift cluster (ClusterVersion 'version' not found)."})
		return
	}
	v, _, _ := unstructured.NestedString(cv.Object, "status", "desired", "version")
	r.Suggested.HostVersion = v
	r.add(Result{Check: "OpenShift version", Status: Pass, Detail: v})
}

// hostMinor returns "4.22" from "4.22.0-0.nightly-...".
func hostMinor(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

func (e *Env) checkNetworks(ctx context.Context, r *Report, req Requirements) {
	nw, err := e.Dyn.Resource(GVRNetwork).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		r.add(Result{Check: "Network overlap", Status: Warn, Detail: "cannot read network.config/cluster: " + err.Error()})
		return
	}
	var host []string
	cn, _, _ := unstructured.NestedSlice(nw.Object, "spec", "clusterNetwork")
	for _, c := range cn {
		if m, ok := c.(map[string]interface{}); ok {
			if cidr, ok := m["cidr"].(string); ok {
				host = append(host, cidr)
			}
		}
	}
	sn, _, _ := unstructured.NestedStringSlice(nw.Object, "spec", "serviceNetwork")
	host = append(host, sn...)

	guest := []string{req.GuestMachineCIDR, req.GuestClusterCIDR, req.GuestServiceCIDR}
	conflicts, err := FindCIDRConflicts(guest, host)
	switch {
	case err != nil:
		r.add(Result{Check: "Network overlap", Status: Warn, Detail: err.Error()})
	case len(conflicts) > 0:
		var parts []string
		for _, c := range conflicts {
			parts = append(parts, fmt.Sprintf("guest %s overlaps host %s", c.Guest, c.Host))
		}
		r.add(Result{Check: "Network overlap", Status: Fail, Detail: strings.Join(parts, "; "),
			Hint: "Guests must not reuse the host's pod/service ranges, or they cannot reach host Services (DNS, S3) through NAT. Pick different guest CIDRs."})
	default:
		r.add(Result{Check: "Network overlap", Status: Pass,
			Detail: fmt.Sprintf("host %s vs guest %s", strings.Join(host, ","), strings.Join(guest, ","))})
	}
}

func (e *Env) checkHostDNS(ctx context.Context, r *Report) {
	svc, err := e.Kube.CoreV1().Services("openshift-dns").Get(ctx, "dns-default", metav1.GetOptions{})
	if err != nil {
		r.add(Result{Check: "Host DNS service", Status: Warn, Detail: err.Error(),
			Hint: "dnsmasq forwards non-vSNO names to the host DNS Service; set it manually."})
		return
	}
	r.Suggested.HostDNS = svc.Spec.ClusterIP
	r.add(Result{Check: "Host DNS service", Status: Pass, Detail: "openshift-dns/dns-default " + svc.Spec.ClusterIP})
}

// channels returns (defaultChannel, channel names) of a package manifest.
func (e *Env) channels(ctx context.Context, pkg string) (string, []string, error) {
	pm, err := e.Dyn.Resource(GVRPackageManifest).Namespace(marketplaceNS).Get(ctx, pkg, metav1.GetOptions{})
	if err != nil {
		return "", nil, err
	}
	def, _, _ := unstructured.NestedString(pm.Object, "status", "defaultChannel")
	chs, _, _ := unstructured.NestedSlice(pm.Object, "status", "channels")
	var names []string
	for _, c := range chs {
		if m, ok := c.(map[string]interface{}); ok {
			if n, ok := m["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	return def, names, nil
}

func (e *Env) checkCatalog(ctx context.Context, r *Report) {
	def, chs, err := e.channels(ctx, "kubevirt-hyperconverged")
	if err != nil {
		r.add(Result{Check: "Catalog: OpenShift Virtualization", Status: Fail, Detail: err.Error(),
			Hint: "kubevirt-hyperconverged must be available in openshift-marketplace (redhat-operators catalog)."})
	} else {
		r.Suggested.CNVChannel = def
		r.add(Result{Check: "Catalog: OpenShift Virtualization", Status: Pass,
			Detail: fmt.Sprintf("kubevirt-hyperconverged (default %s; %s)", def, strings.Join(chs, ","))})
	}

	def, chs, err = e.channels(ctx, "lvms-operator")
	if err != nil {
		r.add(Result{Check: "Catalog: LVM Storage", Status: Fail, Detail: err.Error(),
			Hint: "lvms-operator must be available in openshift-marketplace (redhat-operators catalog)."})
		return
	}
	want := "stable-" + hostMinor(r.Suggested.HostVersion)
	ch := def
	for _, c := range chs {
		if c == want {
			ch = want
		}
	}
	r.Suggested.LVMSChannel = ch
	status, detail := Pass, fmt.Sprintf("lvms-operator channel %s", ch)
	if ch != want && hostMinor(r.Suggested.HostVersion) != "" {
		status = Warn
		detail = fmt.Sprintf("no %s channel; falling back to default %s", want, def)
	}
	r.add(Result{Check: "Catalog: LVM Storage", Status: status, Detail: detail})
}

// existing reports what is already installed. Returns true if LVMS already provides vsno storage.
func (e *Env) checkExisting(ctx context.Context, r *Report) (lvmsReady bool) {
	if csvs, err := e.Dyn.Resource(GVRCSV).Namespace(cnvNS).List(ctx, metav1.ListOptions{}); err == nil && len(csvs.Items) > 0 {
		r.add(Result{Check: "Existing: OpenShift Virtualization", Status: Info,
			Detail: "already installed (" + csvs.Items[0].GetName() + "); setup will reuse it"})
	}
	if lc, err := e.Dyn.Resource(GVRLVMCluster).Namespace(storageNS).List(ctx, metav1.ListOptions{}); err == nil && len(lc.Items) > 0 {
		r.add(Result{Check: "Existing: LVM Storage", Status: Info,
			Detail: fmt.Sprintf("%d LVMCluster(s) present (%s)", len(lc.Items), lc.Items[0].GetName())})
	}
	if _, err := e.Kube.StorageV1().StorageClasses().Get(ctx, vsnoSC, metav1.GetOptions{}); err == nil {
		lvmsReady = true
		r.add(Result{Check: "Existing: vsno storage", Status: Info, Detail: "StorageClass " + vsnoSC + " exists; no new disk needed"})
	}
	if e.Namespace != "" {
		if _, err := e.Kube.CoreV1().Namespaces().Get(ctx, e.Namespace, metav1.GetOptions{}); err == nil {
			r.add(Result{Check: "Existing: vsno environment", Status: Info,
				Detail: "namespace " + e.Namespace + " exists; use `vsno status` to inspect it"})
		}
	}
	return lvmsReady
}

func (e *Env) checkZTP(ctx context.Context, r *Report) {
	_, err := e.Kube.CoreV1().Namespaces().Get(ctx, ztpAgentNS, metav1.GetOptions{})
	if err == nil {
		r.add(Result{Check: "Hub-managed cluster", Status: Warn,
			Detail: "ACM klusterlet present (" + ztpAgentNS + ")",
			Hint:   "Hub policies may flag or revert operators vsno installs. Check the cluster's policies before running setup."})
	} else if !apierrors.IsNotFound(err) {
		r.add(Result{Check: "Hub-managed cluster", Status: Warn, Detail: err.Error()})
	}
}

func (e *Env) checkSeedRepo(r *Report, req Requirements) {
	if req.SeedRepo == "" {
		r.add(Result{Check: "Seed registry", Status: Info,
			Detail: "in-cluster registry (default); pass --seed-repo to push to an external registry instead"})
		return
	}
	file, key, err := FindRegistryAuth(req.SeedRepo, e.AuthFiles)
	if err != nil {
		r.add(Result{Check: "Seed registry", Status: Warn, Detail: "no push credentials for " + req.SeedRepo,
			Hint: "Run `podman login " + repoPrefix(req.SeedRepo) + "` with an account that can push there."})
		return
	}
	r.add(Result{Check: "Seed registry", Status: Pass, Detail: fmt.Sprintf("%s (key %q in %s)", req.SeedRepo, key, file)})
}

func repoPrefix(repo string) string {
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		return repo[:i]
	}
	return repo
}

// candidateWorkers returns Ready, schedulable nodes with the worker role.
func candidateWorkers(nodes []corev1.Node) []corev1.Node {
	var out []corev1.Node
	for _, n := range nodes {
		if _, ok := n.Labels["node-role.kubernetes.io/worker"]; !ok || n.Spec.Unschedulable {
			continue
		}
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		noSchedule := false
		for _, t := range n.Spec.Taints {
			if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
				noSchedule = true
			}
		}
		if ready && !noSchedule {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// freeMemory returns node allocatable memory minus the memory requests of its
// running pods, and separately how much of those requests belong to pods in the
// vsno namespace (an existing environment that vsno would replace or reuse).
func (e *Env) freeMemory(ctx context.Context, node corev1.Node) (free, heldByVsno int64, err error) {
	alloc := node.Status.Allocatable.Memory().Value()
	pods, err := e.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node.Name})
	if err != nil {
		return 0, 0, err
	}
	var used int64
	for _, p := range pods.Items {
		if p.Spec.NodeName != node.Name || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		var req int64
		for _, c := range p.Spec.Containers {
			if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				req += q.Value()
			}
		}
		used += req
		if e.Namespace != "" && p.Namespace == e.Namespace {
			heldByVsno += req
		}
	}
	return alloc - used, heldByVsno, nil
}

// hasTopoLVM reports whether LVMS (TopoLVM CSI) serves storage on this node.
func (e *Env) hasTopoLVM(ctx context.Context, node string) bool {
	csi, err := e.Kube.StorageV1().CSINodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, d := range csi.Spec.Drivers {
		if d.Name == "topolvm.io" {
			return true
		}
	}
	return false
}

// NodeVerdict is the evaluation of one worker.
type NodeVerdict struct {
	Node     string
	Results  []Result
	Eligible bool
	Disk     *Disk // best empty disk, if any
	Score    int
}

// EvaluateNode applies the requirements to one node's hardware and free memory.
// It is pure so it can be tested against captured fixtures.
func EvaluateNode(node string, hw NodeHardware, freeMemBytes int64, req Requirements, storageReady bool) NodeVerdict {
	v := &NodeVerdict{Node: node, Eligible: true}
	v.evalVirt(hw)
	v.evalCPU(hw, req)
	v.evalMemory(freeMemBytes, req)
	v.evalDisk(hw, req, storageReady)
	return *v
}

func (v *NodeVerdict) add(check string, st Status, detail, hint string) {
	v.Results = append(v.Results, Result{Check: v.Node + ": " + check, Status: st, Detail: detail, Hint: hint})
	if st == Fail {
		v.Eligible = false
	}
}

func (v *NodeVerdict) evalVirt(hw NodeHardware) {
	switch {
	case !hw.KVM:
		v.add("virtualization", Fail, fmt.Sprintf("no /dev/kvm (virt=%s, %s)", hw.Virt, hw.Product),
			"Needs a bare-metal node, or a VM with nested virtualization enabled.")
	case hw.BareMetal():
		v.Score += 100
		v.add("virtualization", Pass, fmt.Sprintf("bare metal (%s), /dev/kvm present", hw.Product), "")
	default:
		v.add("virtualization", Warn, fmt.Sprintf("node is itself a VM (virt=%s) with nested KVM", hw.Virt),
			"Works, but nested guests are much slower. Prefer a bare-metal worker.")
	}
}

func (v *NodeVerdict) evalCPU(hw NodeHardware, req Requirements) {
	needThreads := req.VMs * req.VMCores
	switch {
	case hw.Threads >= needThreads:
		v.Score += 10
		v.add("cpu", Pass, fmt.Sprintf("%d threads (need %d for %d VMs)", hw.Threads, needThreads, req.VMs), "")
	case hw.Threads >= req.VMCores:
		v.add("cpu", Warn, fmt.Sprintf("%d threads: enough for one %d-core VM at a time", hw.Threads, req.VMCores),
			"Halt the seed VM after seed generation before creating the target, or lower --vm-cores.")
	default:
		v.add("cpu", Fail, fmt.Sprintf("%d threads; one VM needs %d", hw.Threads, req.VMCores), "")
	}
}

func (v *NodeVerdict) evalMemory(freeMemBytes int64, req Requirements) {
	perVM := (req.VMMemGiB + vmOverheadGiB) << 30
	freeGiB := freeMemBytes >> 30
	switch {
	case freeMemBytes >= int64(req.VMs)*perVM:
		v.Score += 10
		v.add("memory", Pass, fmt.Sprintf("%d GiB schedulable (need %d GiB)", freeGiB, int64(req.VMs)*(perVM>>30)), "")
	case freeMemBytes >= perVM:
		v.add("memory", Warn, fmt.Sprintf("%d GiB schedulable: enough for one VM at a time", freeGiB),
			"Halt the seed VM after seed generation, or lower --vm-memory.")
	default:
		v.add("memory", Fail, fmt.Sprintf("%d GiB schedulable; one VM needs %d GiB", freeGiB, perVM>>30), "")
	}
}

// pickDisk returns the largest empty disk of at least minGB, plus the names of
// disks that were too small or already LVM members (for the report).
func pickDisk(disks []Disk, minGB int64) (best *Disk, tooSmall, lvm []string) {
	for i := range disks {
		d := disks[i]
		switch {
		case d.LVMMember():
			lvm = append(lvm, d.Name)
		case !d.Empty():
		case d.SizeGB() < minGB:
			tooSmall = append(tooSmall, fmt.Sprintf("%s %dGB", d.Name, d.SizeGB()))
		case best == nil || d.SizeBytes > best.SizeBytes:
			best = &disks[i]
		}
	}
	return best, tooSmall, lvm
}

func (v *NodeVerdict) evalDisk(hw NodeHardware, req Requirements, storageReady bool) {
	best, tooSmall, lvm := pickDisk(hw.Disks, req.MinDiskGB)
	switch {
	case best != nil:
		v.Disk = best
		id := best.PreferredByID()
		if id == "" {
			id = "/dev/" + best.Name + " (no stable by-id name found)"
		}
		v.add("disk", Pass, fmt.Sprintf("%s %dGB empty: %s", best.Name, best.SizeGB(), id), "")
	case storageReady:
		v.add("disk", Info, fmt.Sprintf("no free disk, but StorageClass %s already exists (LVM members: %s)", vsnoSC, strings.Join(lvm, ",")), "")
	default:
		detail := "no empty disk"
		if len(tooSmall) > 0 {
			detail += fmt.Sprintf(" of at least %dGB (too small: %s)", req.MinDiskGB, strings.Join(tooSmall, ", "))
		}
		if len(lvm) > 0 {
			detail += "; LVM members: " + strings.Join(lvm, ",")
		}
		v.add("disk", Fail, detail, "LVMS needs a whole, unpartitioned disk with no filesystem signatures.")
	}
}
