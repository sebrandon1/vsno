package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/registry"
	"github.com/sebrandon1/vsno/lib/release"
)

// DefaultEstimate is how much a release mirror takes when none has been
// measured yet. A payload is 15-20 GB, so err on the high side before filling
// the volume.
const DefaultEstimate = 20 << 30

// ImageDigestSource is one install-config imageDigestSources entry: a
// repository the payload pulls from, and the mirror that replaces it.
type ImageDigestSource struct {
	Source  string   `json:"source"`
	Mirrors []string `json:"mirrors"`
}

// Prepared is what a vSNO's install needs once its release is cached.
type Prepared struct {
	// Hit is true when the release was already mirrored.
	Hit bool `json:"hit"`
	// Entry is the cached release.
	Entry `json:"entry"`
	// PullSecret is the cluster's pull secret with the registry's credentials
	// added: the guest pulls from the registry during its install.
	PullSecret string `json:"-"`
	// Sources are the install-config's imageDigestSources.
	Sources []ImageDigestSource `json:"sources"`
	// TrustBundle is the registry's CA, for install-config's
	// additionalTrustBundle.
	TrustBundle string `json:"-"`
	// Elapsed is how long the mirror took; zero for a hit.
	Elapsed time.Duration `json:"-"`
	// Running is true while a mirror is in flight, whether this command
	// started it or found it started.
	Running bool `json:"running,omitempty"`
	// EstimatedBytes is what mirroring is expected to take, for progress.
	EstimatedBytes int64 `json:"estimatedBytes,omitempty"`
}

// EnsureOptions are the user's choices for mirroring a release.
type EnsureOptions struct {
	Namespace string
	Release   release.Release
	// OnLine, if set, gets the mirror's progress lines.
	OnLine func(string)
}

// Ensure mirrors o.Release into the environment's registry unless it is there
// already, waiting for the mirror when it has to, and returns what a vSNO
// needs to install from the mirror. create and run use it: both need the
// release ready before they go on.
func Ensure(ctx context.Context, e Env, o EnsureOptions) (*Prepared, error) {
	p, err := Start(ctx, e, o)
	if err != nil {
		return nil, err
	}
	return Wait(ctx, e, o, p)
}

// Start begins mirroring o.Release and returns at once: the mirror runs in a
// Job in the environment, so it outlives the command that asked for it. What
// `vsno cache add` does without --watch.
//
// A release that is already cached comes back as a hit with no Job, and one
// whose mirror is still running comes back Running, naming that Job, so a
// second call follows the mirror instead of starting another.
func Start(ctx context.Context, e Env, o EnsureOptions) (*Prepared, error) {
	e.defaults()
	t, err := resolveTarget(ctx, e, o)
	if err != nil {
		return nil, err
	}
	m, err := load(ctx, e.Kube, o.Namespace)
	if err != nil {
		return nil, err
	}
	cur := m.Entries[t.digest]
	prev, hit, running, err := e.existing(ctx, o.Namespace, cur, t.reg)
	if err != nil {
		return nil, err
	}
	p := &Prepared{Hit: hit, EstimatedBytes: estimate(m)}
	switch {
	case hit:
		ent := measured(ctx, e, t.reg, prev) // keep what it was mirrored as, not what this request calls it
		// Entries mirrored before signatures were copied get theirs here, and
		// signatures already there cost one listing of the mirror's tags.
		sigs, err := copySignatures(ctx, e, o, t.reg, t.addr, t.repo, t.rel)
		if err != nil {
			return nil, err
		}
		o.line("signatures: " + sigs)
		ent.LastUsed = e.Now()
		ent.Requested = appendUnique(oldRequested(cur), o.Release.Requested)
		if err := saveEntry(ctx, e, o.Namespace, t.digest, ent); err != nil {
			return nil, err
		}
		p.Entry = ent
	case running:
		p.Entry, p.Running = prev, true
	default:
		ent, err := startMirror(ctx, e, o, t, oldRequested(cur), m)
		if err != nil {
			return nil, err
		}
		p.Entry, p.Running = ent, true
	}
	return prepared(ctx, e, t.reg, t.addr, t.repo, t.sources, p)
}

// target is what mirroring a release means: the payload, the registry as the
// mirror and guests reach it, the repository it lands in, and the sources to
// map to it.
type target struct {
	reg     registry.Registry
	rel     payload
	digest  string
	addr    string
	repo    string
	sources []string
}

// resolveTarget reads the release's payload and the environment's registry.
func resolveTarget(ctx context.Context, e Env, o EnsureOptions) (target, error) {
	var t target
	var err error
	if t.reg, err = registry.Load(ctx, e.Kube, o.Namespace); err != nil {
		return t, err
	}
	if t.rel, err = describe(ctx, e, o.Release.Image); err != nil {
		return t, err
	}
	t.digest, t.sources = t.rel.Digest, t.rel.Sources
	if len(t.sources) == 0 {
		return t, fmt.Errorf("release %s lists no images to mirror", o.Release.Image)
	}
	if t.addr, err = registry.Address(ctx, e.Kube, o.Namespace); err != nil {
		return t, err
	}
	t.repo = RepoPrefix + short(t.digest)
	// Both the payload and the release image itself are mapped to the mirror.
	// The agent installer reads the release image before it registers the
	// cluster, from a container that trusts only the registry's CA, so it can
	// not read it from upstream; and the guest's kubelet fetches the image's
	// signature from whichever registry the pull resolves to
	// (use-sigstore-attachments), which is why the signature is copied too.
	// The release's references name only the payload's repository, so the
	// release image's own is added here.
	if r, ok := repoOf(o.Release.Image); ok && !slices.Contains(t.sources, r) {
		t.sources = append(t.sources, r)
		slices.Sort(t.sources)
	}
	return t, nil
}

// existing is the release's entry as it stands: settled when its Job finished
// unwatched, still running when a mirror is in flight, and a hit only when the
// registry still has it. The ConfigMap can outlive what it describes (a prune
// that failed halfway, a manual delete), which is why a hit is checked.
func (e Env) existing(ctx context.Context, ns string, cur *Entry, reg registry.Registry) (prev Entry, hit, running bool, err error) {
	if cur == nil {
		return prev, false, false, nil
	}
	prev = *cur
	if running = prev.State == StateMirroring; running {
		if prev, _, err = settle(ctx, e, ns, prev.Digest, prev, reg); err != nil {
			return prev, false, false, err
		}
		running = prev.State == StateMirroring
	}
	hit = prev.State == "" && mirrored(ctx, e, reg, &prev)
	return prev, hit, running, nil
}

// Wait follows the mirror Start began or found running, telling o.OnLine how
// it is going, and returns once the release is usable.
func Wait(ctx context.Context, e Env, o EnsureOptions, p *Prepared) (*Prepared, error) {
	if !p.Running {
		return p, nil
	}
	e.defaults()
	reg, err := registry.Load(ctx, e.Kube, o.Namespace)
	if err != nil {
		return nil, err
	}
	start := p.Started
	stop := e.reportProgress(ctx, o, p.BaselineBytes, p.EstimatedBytes, start)
	defer stop()
	for {
		ent, why, err := settle(ctx, e, o.Namespace, p.Digest, p.Entry, reg)
		if err != nil {
			return nil, err
		}
		p.Entry = ent
		if why != "" {
			p.Running = false
			return p, errors.New(why)
		}
		if ent.State != StateMirroring {
			break
		}
		select {
		case <-ctx.Done():
			return p, ctx.Err()
		case <-time.After(e.poll()):
		}
	}
	p.Running = false
	p.Elapsed = e.Now().Sub(start)
	if p.Signatures != "" {
		o.line("signatures: " + p.Signatures)
	}
	return p, nil
}

// startMirror checks the registry has room, records the release as mirroring,
// and starts the Job that copies the payload and its signatures. The entry is
// written first: if anything after it fails, the record says a mirror started
// and `cache list` shows the failure.
func startMirror(ctx context.Context, e Env, o EnsureOptions, t target, requested []string, m *metadata) (Entry, error) {
	used, total, err := e.Volume(ctx, o.Namespace)
	if err != nil {
		return Entry{}, err
	}
	est := estimate(m)
	free := total - used
	if total > 0 && free < est {
		return Entry{}, fmt.Errorf("the registry volume has %s free and mirroring %s needs about %s: run `vsno cache prune`, or make the volume bigger with `vsno setup --registry-size`",
			bytesText(free), o.Release.Requested, bytesText(est))
	}
	if total > 0 && free < est+est/4 {
		o.line(fmt.Sprintf("the registry has %s free: mirroring %s needs about %s", bytesText(free), o.Release.Requested, bytesText(est)))
	}
	files, err := credentials(ctx, e, t.reg, t.addr)
	if err != nil {
		return Entry{}, err
	}
	if files["images.txt"], err = imagesFile(o, t.rel); err != nil {
		return Entry{}, err
	}
	start := e.Now()
	ent := Entry{Version: o.Release.Version, Image: o.Release.Image, Digest: t.digest, Repo: t.repo,
		Mirrored: start, Started: start, State: StateMirroring, Requested: requested,
		BaselineBytes: used, Job: mirrorJob(t.digest)}
	if err := saveEntry(ctx, e, o.Namespace, t.digest, ent); err != nil {
		return ent, err
	}
	if err := startMirrorJob(ctx, e, o.Namespace, t.digest, files, o.Release.Image, t.reg.Host, t.repo, short(t.digest), t.addr); err != nil {
		ent.State, ent.Started, ent.BaselineBytes = StateFailed, time.Time{}, 0
		_ = saveEntry(context.WithoutCancel(ctx), e, o.Namespace, t.digest, ent) // best effort: the release is not usable
		return ent, err
	}
	return ent, nil
}

// settle reads the Job behind a mirroring entry. A Job that has finished
// becomes the entry's result — ready with the release's size and what it copied
// of its signatures, or failed — and a Job that is gone has failed: nothing is
// left to finish the mirror. The second return is why it failed, already
// recorded in the entry.
func settle(ctx context.Context, e Env, ns, digest string, ent Entry, reg registry.Registry) (Entry, string, error) {
	job := ent.Job
	if job == "" {
		job = mirrorJob(digest)
	}
	found, done, msg, err := e.mirrorJobState(ctx, ns, job)
	switch {
	case err != nil:
		return ent, "", err
	case !found:
		return e.failMirror(ctx, ns, digest, ent,
			"the mirror of "+versionOf(ent)+" stopped without saying why: its Job "+job+" is gone; run `vsno cache add` again")
	case !done:
		return ent, "", nil
	}
	logs := jobLog(ctx, e, ns, job, 0)
	if msg != "" {
		return e.failMirror(ctx, ns, digest, ent,
			"mirroring "+versionOf(ent)+": Job "+job+" failed"+logTail(logs))
	}
	ent.State, ent.Started, ent.BaselineBytes = "", time.Time{}, 0
	ent.Signatures = sigsFromLog(logs)
	if n, err := repoSize(ctx, e, reg, ent.Repo); err == nil {
		ent.SizeBytes, ent.Measured = n, true
	}
	ent.Job = ""
	if err := saveEntry(ctx, e, ns, digest, ent); err != nil {
		return ent, "", err
	}
	removeMirrorJob(ctx, e, ns, digest) // the log and its credentials are no longer needed
	return ent, "", nil
}

// failMirror records that a release's mirror did not finish, keeping the Job's
// name for the command that reports it.
func (e Env) failMirror(ctx context.Context, ns, digest string, ent Entry, why string) (Entry, string, error) {
	ent.State, ent.Started, ent.BaselineBytes = StateFailed, time.Time{}, 0
	_ = saveEntry(ctx, e, ns, digest, ent) // best effort: the release is not usable
	return ent, why, nil
}

// Reconcile records the result of mirrors whose Jobs finished while no command
// was watching, so `cache list` shows what really happened rather than a mirror
// that ended hours ago.
func Reconcile(ctx context.Context, e Env, ns string) error {
	m, err := load(ctx, e.Kube, ns)
	if err != nil {
		return err
	}
	var reg registry.Registry
	loaded := false
	for digest, ent := range m.Entries {
		if ent.State != StateMirroring {
			continue
		}
		if !loaded {
			if reg, err = registry.Load(ctx, e.Kube, ns); err != nil {
				return err
			}
			loaded = true
		}
		if _, _, err := settle(ctx, e, ns, digest, *ent, reg); err != nil {
			return err
		}
	}
	return nil
}

// versionOf is the entry's version, or its image when it has none.
func versionOf(ent Entry) string {
	if ent.Version != "" {
		return ent.Version
	}
	return ent.Image
}

// logTail is a log's last line as ": line", for an error; "" for no log.
func logTail(logs string) string {
	if l := builder.LastLine(logs); l != "" {
		return ": " + l
	}
	return ""
}

// prepared fills in what a guest needs to install from the mirror: the pull
// secret with the registry's login, its CA, and every source mapped to repo.
func prepared(ctx context.Context, e Env, reg registry.Registry, addr, repo string, sources []string, p *Prepared) (*Prepared, error) {
	ps, err := pullSecret(ctx, e.Kube)
	if err != nil {
		return nil, err
	}
	if p.PullSecret, err = registry.AddAuth(ps, addr, reg.User, reg.Password); err != nil {
		return nil, err
	}
	p.TrustBundle = reg.CA
	for _, s := range sources {
		p.Sources = append(p.Sources, ImageDigestSource{Source: s, Mirrors: []string{addr + "/" + repo}})
	}
	return p, nil
}

// saveEntry writes one entry into the cache's record.
func saveEntry(ctx context.Context, e Env, ns, digest string, ent Entry) error {
	return update(ctx, e.Kube, ns, func(m *metadata) { m.Entries[digest] = &ent })
}

// mirrored reports whether a cached entry's images are still in the registry.
func mirrored(ctx context.Context, e Env, reg registry.Registry, ent *Entry) bool {
	out, err := reg.Run(ctx, e.Builder, reg.Host, "exists", ent.Repo+":"+short(ent.Digest))
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

// measured is ent with its size counted from blobs, if it was recorded before
// sizes were. It is best effort: a hit must not fail over its size.
func measured(ctx context.Context, e Env, reg registry.Registry, ent Entry) Entry {
	if !ent.Measured {
		if n, err := repoSize(ctx, e, reg, ent.Repo); err == nil {
			ent.SizeBytes, ent.Measured = n, true
		}
	}
	return ent
}

// repoSize is how many bytes repo's images take in the registry, counted from
// the blobs they reference.
func repoSize(ctx context.Context, e Env, reg registry.Registry, repo string) (int64, error) {
	out, err := reg.Run(ctx, e.Builder, reg.Host, "size", repo)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("reading %s's size: %w", repo, err)
	}
	return n, nil
}

// credentials is the tar mirror.sh and signature.sh read on stdin: the cluster
// pull secret with the registry's login added, and the registry's CA.
func credentials(ctx context.Context, e Env, reg registry.Registry, addr string) (map[string]string, error) {
	ps, err := pullSecret(ctx, e.Kube)
	if err != nil {
		return nil, err
	}
	auth, err := registry.AddAuth(ps, addr, reg.User, reg.Password)
	if err != nil {
		return nil, err
	}
	return map[string]string{"auth.json": auth, "ca.crt": reg.CA}, nil
}

// sigsRe reads signature.sh's summary.
var sigsRe = regexp.MustCompile(`^copied \d+, present \d+, none \d+$`)

// imagesFile is signature.sh's images.txt: the release image and every
// component image, one repository@digest per line.
func imagesFile(o EnsureOptions, rel payload) (string, error) {
	src, ok := repoOf(o.Release.Image)
	if !ok {
		return "", fmt.Errorf("release image %q names no repository", o.Release.Image)
	}
	return strings.Join(append([]string{src + "@" + rel.Digest}, rel.Images...), "\n") + "\n", nil
}

// copySignatures puts the sigstore signature of the release image and of each
// of its component images next to their mirrored copies, and says how many
// it copied, found there already, or found none of upstream (nightlies have
// none). A guest pulls from the mirror and looks for signatures there
// (use-sigstore-attachments); from 4.22 its policy requires them for the
// component images too, and without them the node never starts its kubelet.
//
// A fresh mirror copies them inside its Job, so this runs for a release that
// was cached before the signatures were: it is one listing of the mirror's
// tags when they are all there.
func copySignatures(ctx context.Context, e Env, o EnsureOptions, reg registry.Registry, addr, repo string, rel payload) (string, error) {
	files, err := credentials(ctx, e, reg, addr)
	if err != nil {
		return "", err
	}
	if files["images.txt"], err = imagesFile(o, rel); err != nil {
		return "", err
	}
	tarred, err := builder.Tar(files)
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	err = e.Builder.Run(ctx, script("signature.sh"), []string{addr, repo},
		builder.IO{Stdin: bytes.NewReader(tarred), Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return "", fmt.Errorf("copying %s's signatures into the mirror: %w%s", o.Release.Requested, err, builder.Tail(stderr.String()))
	}
	out := strings.TrimSpace(stdout.String())
	if !sigsRe.MatchString(out) {
		return "", fmt.Errorf("copying %s's signatures into the mirror: unexpected output %q", o.Release.Requested, out)
	}
	return out, nil
}

// reportProgress says how much the mirror has written, and how fast, while it
// runs: the registry's volume is the only measure of what a mirror has pushed.
// The returned function stops it.
func (e Env) reportProgress(ctx context.Context, o EnsureOptions, base, est int64, start time.Time) func() {
	if o.OnLine == nil {
		return func() {}
	}
	every := e.Progress
	if every <= 0 {
		every = 5 * time.Second
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				used, _, err := e.Volume(ctx, o.Namespace)
				if err != nil || used <= base {
					continue
				}
				o.line(fmt.Sprintf("mirrored %s of about %s (%s)", bytesText(used-base), bytesText(est), Rate(used-base, e.Now().Sub(start))))
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// Rate is bytes per second, as "12MiB/s".
func Rate(bytes int64, d time.Duration) string {
	if d <= 0 || bytes <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0fMiB/s", float64(bytes)/d.Seconds()/(1<<20))
}

func (o EnsureOptions) line(s string) {
	if o.OnLine != nil {
		o.OnLine(s)
	}
}

// estimate is what a mirror takes: the largest one recorded, or the default.
func estimate(m *metadata) int64 {
	est := int64(0)
	for _, ent := range m.Entries {
		if ent.SizeBytes > est {
			est = ent.SizeBytes
		}
	}
	if est == 0 {
		return DefaultEstimate
	}
	return est
}

// oldRequested is a cached entry's requested forms, if it exists.
func oldRequested(ent *Entry) []string {
	if ent == nil {
		return nil
	}
	return ent.Requested
}

// appendUnique is items with s added once.
func appendUnique(items []string, s string) []string {
	if s == "" || slices.Contains(items, s) {
		return items
	}
	return append(items, s)
}

// bytesText renders a size in GiB or MiB.
func bytesText(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%dMi", n>>20)
}
