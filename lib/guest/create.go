package guest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/registry"
	"github.com/sebrandon1/vsno/lib/release"
	"github.com/sebrandon1/vsno/lib/state"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// Cache mirrors a release into the environment's registry.
type Cache interface {
	Ensure(ctx context.Context, ns string, rel release.Release, onLine func(string)) (*cache.Prepared, error)
}

// CreateOptions are the user's choices for vsno create.
type CreateOptions struct {
	Namespace string
	Name      string
	Version   string
	Node      string // "" = the node vsno setup chose
	// Cores, MemoryGiB and DiskGiB are the vSNO's size; 0 is the default.
	Cores     int
	MemoryGiB int64
	DiskGiB   int64
	// Wait for the install to finish; otherwise return once the VM exists.
	Wait bool
	// NoCache installs from the internet instead of the registry mirror.
	NoCache bool
	// Snapshot restores the root disk from this VolumeSnapshot instead of
	// installing from an ISO. The vSNO takes the snapshot's name and version.
	Snapshot string
	// StuckAfter fails an install whose validations keep failing this long; 0 = 15m.
	StuckAfter time.Duration
}

// Sized fills in the default size for each one o leaves at 0, as vsno
// create's flags do.
func (o CreateOptions) Sized() CreateOptions {
	if o.Cores == 0 {
		o.Cores = DefaultCores
	}
	if o.MemoryGiB == 0 {
		o.MemoryGiB = DefaultMemoryGiB
	}
	if o.DiskGiB == 0 {
		o.DiskGiB = DefaultDiskGiB
	}
	return o
}

// Create builds and installs a vSNO, named randomly if o.Name is empty. It
// returns the vSNO as far as it got.
func Create(ctx context.Context, e Env, o CreateOptions) (*Guest, error) {
	e.defaults()
	start := e.Now()
	if o.Name == "" {
		name, err := FreeName(ctx, e, o.Namespace)
		if err != nil {
			return nil, err
		}
		o.Name = name
	}
	ps := e.Progress.Start("Checking " + o.Name)
	spec, sec, dnsIP, err := prepare(ctx, e, o)
	if err != nil {
		ps.Fail(err.Error())
		return nil, err
	}
	ps.Done(fmt.Sprintf("%s on %s, %d vCPU / %d GiB / %d GiB", describe(spec.Release), spec.Node, spec.Cores, spec.MemoryGiB, spec.DiskGiB))

	cached, err := makeDisk(ctx, e, o, spec, &sec)
	if err != nil {
		ps.Fail(err.Error())
		return nil, err
	}
	objs, err := Objects(spec, dnsIP, sec)
	if err != nil {
		return nil, err
	}
	g := &Guest{Name: spec.Name, State: Starting, Version: spec.Release.Version, Requested: spec.Release.Requested,
		Image: spec.Release.Image, Node: spec.Node, Cores: int64(spec.Cores), MemoryGiB: spec.MemoryGiB, DiskGiB: spec.DiskGiB,
		API: API(spec.Name), Cached: cached}

	// The Service first: the builder reaches the guest through its ClusterIP.
	if err := step(e, "Service and DNS for "+spec.Name, func(precheck.ProgressStep) (string, error) {
		if _, err := e.Applier.Apply(ctx, objs[:1]); err != nil {
			return "", err
		}
		return "api." + spec.Name + "." + manifests.DefaultBaseDomain, SetDNS(ctx, e, spec.Namespace, spec.Name, true)
	}); err != nil {
		return g, err
	}

	if err := boot(ctx, e, spec, sec, dnsIP, g, objs); err != nil {
		return g, err
	}

	started := e.Now()
	if err := step(e, "Creating VM "+spec.Name, func(precheck.ProgressStep) (string, error) {
		_, err := e.Applier.Apply(ctx, objs[1:])
		if spec.Snapshot != "" {
			return fmt.Sprintf("root disk %d GiB from snapshot %s", spec.DiskGiB, spec.Snapshot), err
		}
		return fmt.Sprintf("ISO DataVolume %s-iso, root disk %d GiB", spec.Name, spec.DiskGiB), err
	}); err != nil {
		return g, err
	}
	if !o.Wait {
		return g, record(ctx, e, spec.Namespace, g)
	}
	if err = waitInstalled(ctx, e, g, o, started); err != nil {
		return g, err
	}
	// The whole create, caching and ISO build included: the number to compare
	// a cached release's create against.
	g.Timings.Available = dur(e.Now().Sub(start))
	if spec.Snapshot == "" {
		eject(ctx, e, spec.Namespace, spec.Name)
	}
	return g, record(ctx, e, spec.Namespace, g)
}

// makeDisk makes what the vSNO's VM boots from: the root disk restored from a
// snapshot, or the release mirrored for an install. It reports whether the
// install uses the environment's registry.
func makeDisk(ctx context.Context, e Env, o CreateOptions, spec Spec, sec *Secrets) (bool, error) {
	if spec.Snapshot != "" {
		return false, restore(ctx, e, spec)
	}
	return cacheRelease(ctx, e, o, spec, sec)
}

// boot is what the VM starts from: a restore of the snapshot's root disk, which
// restore has already made, or the installer ISO built and applied.
func boot(ctx context.Context, e Env, spec Spec, sec Secrets, dnsIP string, g *Guest, objs []*unstructured.Unstructured) error {
	if spec.Snapshot != "" {
		return nil
	}
	isoStart := e.Now()
	return step(e, "Building the agent ISO", func(ps precheck.ProgressStep) (string, error) {
		v, err := buildISO(ctx, e, spec, dnsIP, sec, ps)
		if spec.Release.Version == "" && v != "" {
			g.Version = v
			ann := objs[2].GetAnnotations() // a copy
			ann[AnnVersion] = v             // a pullspec: the installer knows the version
			objs[2].SetAnnotations(ann)
		}
		// The build alone: prepare and the release mirror are their own time.
		g.Timings.ISO = dur(e.Now().Sub(isoStart))
		return v, err
	})
}

// restore makes the boot disk for a vSNO created from a snapshot: a PVC filled
// by the storage driver from the VolumeSnapshot (a thin clone on LVMS, seconds),
// and the installer files the snapshot kept, which the probe needs after a
// `vsno delete` removed them.
func restore(ctx context.Context, e Env, s Spec) error {
	if err := step(e, "Restoring the disk", func(precheck.ProgressStep) (string, error) {
		pvc := rootPVC(s)
		if _, err := e.Kube.CoreV1().PersistentVolumeClaims(s.Namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating the root disk: %w", err)
		}
		if e.Builder == nil {
			return "snapshot " + s.Snapshot, nil
		}
		if err := e.Builder.Run(ctx, restoreFiles, []string{s.Name}, builder.IO{}); err != nil {
			return "", fmt.Errorf("restoring the installer files: %w", err)
		}
		return "snapshot " + s.Snapshot, nil
	}); err != nil {
		return err
	}
	return nil
}

// rootPVC is the vSNO's root disk, restored from its snapshot by the driver.
func rootPVC(s Spec) *corev1.PersistentVolumeClaim {
	block := corev1.PersistentVolumeBlock
	class := manifests.DefaultStorageClass
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name + "-root", Labels: map[string]string{LabelName: s.Name}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			VolumeMode:       &block,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(strconv.Itoa(int(s.DiskGiB)) + "Gi")}},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: ptr("snapshot.storage.k8s.io"), Kind: "VolumeSnapshot", Name: s.Snapshot},
		},
	}
}

// ptr is a pointer to v, for the fields Kube's types take by pointer.
func ptr[T any](v T) *T { return &v }

// snapVersion is the OpenShift version a snapshot's disk was installed at.
func snapVersion(ctx context.Context, e Env, ns, snap string) (string, error) {
	u, err := e.Dyn.Resource(cluster.GVRVolumeSnapshot).Namespace(ns).Get(ctx, snap, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading snapshot %s: %w", snap, err)
	}
	v := u.GetAnnotations()[AnnVersion]
	if v == "" {
		return "", fmt.Errorf("snapshot %s has no version recorded; take it again with `vsno snapshot create`", snap)
	}
	return v, nil
}

// restoreFiles puts the snapshot's copy of the installer files back.
const restoreFiles = `mkdir -p "/work/guests/$1" && cp -a "/work/snapshots/$1/." "/work/guests/$1/"`

// cacheRelease mirrors the release into the environment's registry and points
// the install at it. The ISO build pulls the release too, so this runs before
// it.
func cacheRelease(ctx context.Context, e Env, o CreateOptions, spec Spec, sec *Secrets) (bool, error) {
	if e.Cache == nil || o.NoCache {
		// The mirror is optional, the trust it brings is not: LCA requires a seed
		// image's trust bundle to match its target's, so a vSNO installed without
		// one can only make seeds for vSNOs installed the same way.
		sec.AdditionalTrustBundle = environmentCA(ctx, e, spec.Namespace)
		return false, nil
	}
	err := step(e, "Caching release "+describe(spec.Release), func(ps precheck.ProgressStep) (string, error) {
		p, err := e.Cache.Ensure(ctx, spec.Namespace, spec.Release, ps.Update)
		if err != nil {
			return "", fmt.Errorf("%w (pass --no-cache to install from the internet)", err)
		}
		sec.PullSecret, sec.ImageDigestSources, sec.AdditionalTrustBundle = p.PullSecret, p.Sources, p.TrustBundle
		if p.Hit {
			return "already mirrored: " + p.Repo, nil
		}
		return "mirrored " + p.Repo, nil
	})
	return err == nil, err
}

// environmentCA is the environment registry's CA, or "" when there is none to
// trust: a hand-built environment, or one where setup has not run.
func environmentCA(ctx context.Context, e Env, ns string) string {
	if e.Kube == nil {
		return ""
	}
	reg, err := registry.Load(ctx, e.Kube, ns)
	if err != nil {
		return ""
	}
	return reg.CA
}

// eject drops the install ISO of a vSNO that just became ready, freeing the
// ISO DataVolume and the served ISO. A failure here doesn't fail the create:
// `vsno isos prune` gets it later.
func eject(ctx context.Context, e Env, ns, name string) {
	ps := e.Progress.Start("Ejecting the install ISO")
	if _, err := EjectISO(ctx, e, ns, name); err != nil {
		ps.Warn(err.Error())
		return
	}
	if e.Builder != nil {
		_ = e.Builder.Run(ctx, script("rm.sh"), []string{name}, builder.IO{})
	}
	ps.Done("the vSNO boots from its root disk")
}

func describe(r release.Release) string {
	if r.Version != "" {
		return r.Version
	}
	return r.Image
}

// step runs fn as one progress step.
func step(e Env, title string, fn func(precheck.ProgressStep) (string, error)) error {
	ps := e.Progress.Start(title)
	detail, err := fn(ps)
	if err != nil {
		ps.Fail(err.Error())
		return err
	}
	ps.Done(detail)
	return nil
}

// prepare checks everything Create needs before changing anything.
func prepare(ctx context.Context, e Env, o CreateOptions) (Spec, Secrets, string, error) {
	o = o.Sized()
	s := Spec{Namespace: o.Namespace, Name: o.Name, Node: o.Node, Cores: o.Cores, MemoryGiB: o.MemoryGiB, DiskGiB: o.DiskGiB}
	var sec Secrets
	if err := s.Validate(); err != nil {
		return s, sec, "", err
	}
	if err := checkEnvironment(ctx, e, o.Namespace); err != nil {
		return s, sec, "", err
	}
	switch _, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(o.Namespace).Get(ctx, o.Name, metav1.GetOptions{}); {
	case err == nil:
		return s, sec, "", fmt.Errorf("vSNO %s already exists; `vsno delete %s` first, or pick another name", o.Name, o.Name)
	case !apierrors.IsNotFound(err):
		return s, sec, "", fmt.Errorf("checking for vSNO %s: %w", o.Name, err)
	}
	if s.Node == "" {
		st, _, err := state.Load(ctx, e.Kube, o.Namespace)
		if err != nil {
			return s, sec, "", err
		}
		if s.Node = st.Node; s.Node == "" {
			return s, sec, "", fmt.Errorf("no node recorded for %s; pass --node", o.Namespace)
		}
	}
	if err := checkCapacity(ctx, e, s); err != nil {
		return s, sec, "", err
	}
	var err error
	if s.Snapshot = o.Snapshot; s.Snapshot != "" {
		// A restore brings the release with the disk: no ISO, no mirror, and
		// the version is the one the snapshot was taken at.
		if s.Release.Version, err = snapVersion(ctx, e, o.Namespace, o.Snapshot); err != nil {
			return s, sec, "", err
		}
		s.Release.Requested = s.Release.Version
	} else if s.Release, err = e.Resolver.Resolve(ctx, o.Version); err != nil {
		return s, sec, "", err
	}
	if s.Snapshot == "" {
		if sec, err = secrets(ctx, e, o.Namespace, s.Release); err != nil {
			return s, sec, "", err
		}
	}
	svc, err := e.Kube.CoreV1().Services(o.Namespace).Get(ctx, "dnsmasq", metav1.GetOptions{})
	if err != nil {
		return s, sec, "", fmt.Errorf("reading the dnsmasq Service: %w", err)
	}
	return s, sec, svc.Spec.ClusterIP, nil
}

// checkEnvironment requires an environment vsno set up, with a ready builder.
func checkEnvironment(ctx context.Context, e Env, ns string) error {
	n, err := e.Kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("namespace %s does not exist; run `vsno setup -n %s` first", ns, ns)
	}
	if err != nil {
		return fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	ours, err := cluster.CreatedByVsno(ctx, e.Kube, ns, n.Labels)
	if err != nil {
		return err
	}
	if !ours {
		return fmt.Errorf("namespace %s was not created by vsno setup; create vSNOs in an environment vsno set up (-n)", ns)
	}
	_, err = builder.Pod(ctx, e.Kube, ns)
	return err
}

// ErrNoCapacity matches the error for vSNOs that don't fit their node's memory.
var ErrNoCapacity = errors.New("not enough memory on the node")

// noCapacity says how short the node is; errors.Is matches it to ErrNoCapacity.
type noCapacity string

func (e noCapacity) Error() string      { return string(e) }
func (noCapacity) Is(target error) bool { return target == ErrNoCapacity }

// checkCapacity refuses a vSNO that doesn't fit the node next to what runs there.
func checkCapacity(ctx context.Context, e Env, s Spec) error {
	return Fit(ctx, e, s.Node, s.Cores, s.MemoryGiB, 1)
}

// Fit refuses count vSNOs of cores and memoryGiB each that don't fit node
// next to what runs there, counting memory at the cluster's overcommit. A
// shortage of memory wraps ErrNoCapacity.
func Fit(ctx context.Context, e Env, node string, cores int, memoryGiB int64, count int) error {
	d, err := cluster.ReadDensity(ctx, e.Dyn)
	if err != nil {
		return err
	}
	c, err := NodeCapacity(ctx, e.Kube, node, d.Percent())
	if err != nil {
		return err
	}
	need := int64(count) * (cluster.RequestGiB(memoryGiB, d.Percent()) + vmOverheadGiB)
	switch {
	case int64(cores) > c.Cores:
		return fmt.Errorf("--cores %d is more than node %s has (%d)", cores, node, c.Cores)
	case need > c.FreeMemoryGiB && count == 1:
		return noCapacity(fmt.Sprintf("node %s has %d GiB of memory free; a %d GiB vSNO needs %d GiB. Stop or delete a vSNO (`vsno list`), or use a smaller --memory (min %d)",
			node, c.FreeMemoryGiB, memoryGiB, need, MinMemoryGiB))
	case need > c.FreeMemoryGiB:
		return noCapacity(fmt.Sprintf("node %s has %d GiB of memory free; %d vSNOs of %d GiB need %d GiB",
			node, c.FreeMemoryGiB, count, memoryGiB, need))
	}
	return nil
}

// secrets reads the host pull secret (which must reach the CI registry for a
// nightly) and the builder's SSH public key.
func secrets(ctx context.Context, e Env, ns string, rel release.Release) (Secrets, error) {
	var sec Secrets
	ps, err := e.Kube.CoreV1().Secrets("openshift-config").Get(ctx, "pull-secret", metav1.GetOptions{})
	if err != nil {
		return sec, fmt.Errorf("reading the cluster pull secret: %w", err)
	}
	var auths struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	raw := ps.Data[corev1.DockerConfigJsonKey]
	if err := json.Unmarshal(raw, &auths); err != nil {
		return sec, fmt.Errorf("parsing the cluster pull secret: %w", err)
	}
	if _, ok := auths.Auths[release.CIRegistry]; rel.Nightly && !ok {
		return sec, fmt.Errorf("%s needs credentials for %s, which the cluster pull secret (openshift-config/pull-secret) doesn't have", rel.Requested, release.CIRegistry)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return sec, err
	}
	sec.PullSecret = compact.String()

	key, err := e.Kube.CoreV1().Secrets(ns).Get(ctx, "vsno-ssh", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return sec, fmt.Errorf("secret %s/vsno-ssh is missing; rerun `vsno setup -n %s`", ns, ns)
	}
	if err != nil {
		return sec, fmt.Errorf("reading secret vsno-ssh: %w", err)
	}
	sec.SSHPublicKey = strings.TrimSpace(string(key.Data["id_ed25519.pub"]))
	return sec, nil
}

// SetDNS adds (or removes) vSNO name's file in the guests ConfigMap that
// dnsmasq loads, creating the ConfigMap on first use.
func SetDNS(ctx context.Context, e Env, ns, name string, add bool) error {
	cms := e.Kube.CoreV1().ConfigMaps(ns)
	key := name + ".conf"
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := cms.Get(ctx, manifests.GuestsConfigMap, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err) && !add:
			return nil
		case apierrors.IsNotFound(err):
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: manifests.GuestsConfigMap, Namespace: ns,
				Labels: map[string]string{cluster.ManagedByLabel: "vsno"}}}
			cm.Data = map[string]string{key: manifests.GuestDNS(name, manifests.DefaultBaseDomain)}
			_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
			return err
		case err != nil:
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if add {
			cm.Data[key] = manifests.GuestDNS(name, manifests.DefaultBaseDomain)
		} else {
			if _, ok := cm.Data[key]; !ok {
				return nil
			}
			delete(cm.Data, key)
		}
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

// buildISO runs iso.sh in the builder and returns the release's version as
// openshift-install reports it.
func buildISO(ctx context.Context, e Env, s Spec, dnsIP string, sec Secrets, ps precheck.ProgressStep) (string, error) {
	files, err := InstallFiles(s, dnsIP, sec)
	if err != nil {
		return "", err
	}
	in := map[string]string{"pull-secret.json": sec.PullSecret}
	for p, c := range files {
		in["config/"+p] = c
	}
	tarball, err := Tar(in)
	if err != nil {
		return "", err
	}
	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		err := e.Builder.Run(ctx, script("iso.sh"), []string{s.Name, s.Release.Image},
			builder.IO{Stdin: bytes.NewReader(tarball), Stdout: pw, Stderr: &stderr})
		_ = pw.CloseWithError(err)
		done <- err
	}()
	version := ""
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "version "); ok {
			version = v
		}
		ps.Update(line)
	}
	if err := <-done; err != nil {
		return version, fmt.Errorf("building the ISO: %w%s", err, builder.Tail(stderr.String()))
	}
	return version, nil
}

// errStuck is an install that won't finish by itself.
var errStuck = errors.New("install stuck")

// maxPollErrors is how many polls in a row may fail to read the vSNO before a
// watch gives up: about a minute at the default 10s poll.
const maxPollErrors = 6

// readErrors counts a watch's failed reads of a vSNO in a row.
type readErrors int

// tolerate records a read's error: nil resets the count, and an error is
// returned only once more than maxPollErrors reads in a row have failed.
// The guest carries on whether or not the watch can see it.
func (r *readErrors) tolerate(err error) error {
	if err == nil {
		*r = 0
		return nil
	}
	if *r++; *r > maxPollErrors {
		return err
	}
	return nil
}

// waitInstalled polls until the vSNO is ready, failed, stuck or out of time.
// started is when the VM was created: the timings are measured from there.
func waitInstalled(ctx context.Context, e Env, g *Guest, o CreateOptions, started time.Time) error {
	stuckAfter := o.StuckAfter
	if stuckAfter == 0 {
		stuckAfter = 15 * time.Minute
	}
	ps := e.Progress.Start(waitingTitle(o, g.Name))
	deadline := e.Now().Add(e.Timeout)
	var insufficientSince, rebooted time.Time
	var reads readErrors
	for {
		st, detail, ver, err := observeOne(ctx, e, o.Namespace, g.Name)
		if ferr := reads.tolerate(err); ferr != nil {
			ps.Fail(ferr.Error())
			return ferr
		}
		if err != nil {
			ps.Update("cannot read the vSNO: " + err.Error())
			st, detail = Unknown, ""
		} else {
			g.State, g.Detail = st, detail
			if ver != "" {
				g.Version = ver
			}
			ps.Update(string(st) + " " + detail)
			ps.SetProgress(percent(st, detail), 100)
			markReboot(&rebooted, st, e.Now())
		}
		switch {
		case st == Ready:
			ps.Done(g.Version)
			g.timed(started, rebooted, e.Now())
			return nil
		case st == Failed || st == Stopped:
			ps.Fail(string(st) + ": " + detail)
			return fmt.Errorf("vSNO %s %s: %s; see `vsno list`, or `vsno delete %s` and retry", g.Name, st, detail, g.Name)
		case strings.HasPrefix(detail, "waiting on validations"):
			if insufficientSince.IsZero() {
				insufficientSince = e.Now()
			} else if e.Now().Sub(insufficientSince) > stuckAfter {
				ps.Fail(detail)
				return fmt.Errorf("%w: vSNO %s has been %s for %s", errStuck, g.Name, detail, stuckAfter)
			}
		default:
			insufficientSince = time.Time{}
		}
		if e.Now().After(deadline) {
			ps.Fail("timed out")
			return fmt.Errorf("vSNO %s not ready after %s (last: %s %s); it may still finish, see `vsno list`", g.Name, e.Timeout, st, detail)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.Poll):
		}
	}
}

// observeOne is List for a single vSNO.
func observeOne(ctx context.Context, e Env, ns, name string) (State, string, string, error) {
	vm, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Unknown, "", "", fmt.Errorf("reading vSNO %s: %w", name, err)
	}
	g, o := observe(ctx, e, vm)
	if o.VMStatus == "Running" && g.ServiceIP != "" && (o.ISOPhase == "" || o.ISOPhase == "Succeeded") {
		probes, err := Probes(ctx, e.Builder, map[string]string{name: g.ServiceIP})
		if err == nil {
			o.Probe = probes[name]
		}
	}
	st, detail := Classify(o)
	ver := ""
	if o.Probe != nil && o.Probe.CV != nil {
		ver = o.Probe.CV.Version
	}
	return st, detail, ver, nil
}

// markReboot notes when the guest's API first answered, which is when its
// first reboot is done.
func markReboot(rebooted *time.Time, st State, now time.Time) {
	if st == Finishing && rebooted.IsZero() {
		*rebooted = now
	}
}

// timed fills in how long the install and the reboot took.
func (g *Guest) timed(started, rebooted, ready time.Time) {
	g.Timings.Reboot = dur(ready.Sub(rebooted))
	g.Timings.Install = dur(rebooted.Sub(started))
	if rebooted.IsZero() {
		g.Timings.Install, g.Timings.Reboot = dur(ready.Sub(started)), ""
	}
}

// waitingTitle is what the wait is for: an install, or a start of a disk that
// was restored from a snapshot.
func waitingTitle(o CreateOptions, name string) string {
	if o.Snapshot != "" {
		return "Starting " + name
	}
	return "Installing " + name
}

// record stores what create measured on the VM: the version a pullspec turned
// out to be, whether it installed from the mirror, and how long each phase
// took.
func record(ctx context.Context, e Env, ns string, g *Guest) error {
	ann := map[string]string{AnnVersion: g.Version, AnnTimingISO: g.Timings.ISO, AnnTimingInstall: g.Timings.Install,
		AnnTimingReboot: g.Timings.Reboot, AnnTimingAvailable: g.Timings.Available}
	if g.Cached {
		ann[AnnCached] = "true"
	}
	clean := map[string]string{}
	for k, v := range ann {
		if v != "" {
			clean[k] = v
		}
	}
	if len(clean) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": clean}})
	if err != nil {
		return err
	}
	_, err = e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Patch(ctx, g.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// dur is d rounded to the second, or "" when it wasn't measured.
func dur(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.Round(time.Second).String()
}

var rePercent = regexp.MustCompile(`(\d+)(\.\d+)?%`)

// percent is the progress bar for st: the ISO import, then the installer's
// progress; 0 (no bar) otherwise.
func percent(st State, detail string) int {
	if st != Importing && st != Installing {
		return 0
	}
	m := rePercent.FindStringSubmatch(detail)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
