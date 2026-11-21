package cache

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
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
}

// EnsureOptions are the user's choices for mirroring a release.
type EnsureOptions struct {
	Namespace string
	Release   release.Release
	// OnLine, if set, gets the mirror's progress lines.
	OnLine func(string)
}

// Ensure mirrors o.Release into the environment's registry unless it is there
// already, and returns what a vSNO needs to install from the mirror.
func Ensure(ctx context.Context, e Env, o EnsureOptions) (*Prepared, error) {
	e.defaults()
	reg, err := registry.Load(ctx, e.Kube, o.Namespace)
	if err != nil {
		return nil, err
	}
	rel, err := describe(ctx, e, o.Release.Image)
	if err != nil {
		return nil, err
	}
	digest, sources := rel.Digest, rel.Sources
	if len(sources) == 0 {
		return nil, fmt.Errorf("release %s lists no images to mirror", o.Release.Image)
	}
	addr, err := registry.Address(ctx, e.Kube, o.Namespace)
	if err != nil {
		return nil, err
	}
	repo := RepoPrefix + short(digest)
	// Both the payload and the release image itself are mapped to the mirror.
	// The agent installer reads the release image before it registers the
	// cluster, from a container that trusts only the registry's CA, so it can
	// not read it from upstream; and the guest's kubelet fetches the image's
	// signature from whichever registry the pull resolves to
	// (use-sigstore-attachments), which is why the signature is copied too.
	// The release's references name only the payload's repository, so the
	// release image's own is added here.
	if r, ok := repoOf(o.Release.Image); ok && !slices.Contains(sources, r) {
		sources = append(sources, r)
		slices.Sort(sources)
	}
	m, err := load(ctx, e.Kube, o.Namespace)
	if err != nil {
		return nil, err
	}
	prev := m.Entries[digest]
	// The ConfigMap can outlive what it describes (a prune that failed halfway,
	// a manual delete), so a hit is only a hit if the registry still has it.
	hit := prev != nil && prev.State == "" && mirrored(ctx, e, reg, prev)
	var ent Entry
	var elapsed time.Duration
	if hit {
		ent = measured(ctx, e, reg, *prev) // keep what it was mirrored as, not what this request calls it
	} else if ent, elapsed, err = mirrorEntry(ctx, e, o, reg, addr, repo, short(digest), digest, oldRequested(prev), m); err != nil {
		return nil, err
	}
	// Every use, not only a fresh mirror: entries mirrored before signatures
	// were copied get theirs here, and signatures already there cost one
	// listing of the mirror's tags.
	sigs, err := copySignatures(ctx, e, o, reg, addr, repo, rel)
	if err != nil {
		return nil, err
	}
	o.line("signatures: " + sigs)
	ent.LastUsed = e.Now()
	ent.Requested = appendUnique(oldRequested(m.Entries[digest]), o.Release.Requested)
	if err := saveEntry(ctx, e, o.Namespace, digest, ent); err != nil {
		return nil, err
	}
	return prepared(ctx, e, reg, addr, repo, sources, &Prepared{Hit: hit, Entry: ent, Elapsed: elapsed})
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

// mirrorEntry mirrors the release and records the entry as it goes: mirroring
// before the long part starts (so `cache list` shows it), failed if it breaks,
// ready when it is done.
func mirrorEntry(ctx context.Context, e Env, o EnsureOptions, reg registry.Registry, addr, repo, tag, digest string, requested []string, m *metadata) (Entry, time.Duration, error) {
	start := e.Now()
	ent := Entry{Version: o.Release.Version, Image: o.Release.Image, Digest: digest, Repo: repo,
		Mirrored: start, Started: start, State: StateMirroring, Requested: requested}
	if err := saveEntry(ctx, e, o.Namespace, digest, ent); err != nil {
		return ent, 0, err
	}
	grew, elapsed, err := mirror(ctx, e, o, reg, addr, repo, tag, m)
	if err != nil {
		ent.State, ent.Started = StateFailed, time.Time{}
		_ = saveEntry(ctx, e, o.Namespace, digest, ent) // best effort: the release is not usable
		return ent, elapsed, err
	}
	ent.State, ent.Started, ent.SizeBytes = "", time.Time{}, grew
	if n, err := repoSize(ctx, e, reg, repo); err == nil {
		ent.SizeBytes, ent.Measured = n, true
	}
	return ent, elapsed, nil
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

// copySignatures puts the sigstore signature of the release image and of each
// of its component images next to their mirrored copies, and says how many
// it copied, found there already, or found none of upstream (nightlies have
// none). A guest pulls from the mirror and looks for signatures there
// (use-sigstore-attachments); from 4.22 its policy requires them for the
// component images too, and without them the node never starts its kubelet.
func copySignatures(ctx context.Context, e Env, o EnsureOptions, reg registry.Registry, addr, repo string, rel payload) (string, error) {
	src, ok := repoOf(o.Release.Image)
	if !ok {
		return "", fmt.Errorf("release image %q names no repository", o.Release.Image)
	}
	files, err := credentials(ctx, e, reg, addr)
	if err != nil {
		return "", err
	}
	files["images.txt"] = strings.Join(append([]string{src + "@" + rel.Digest}, rel.Images...), "\n") + "\n"
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

// mirror copies the release's payload into the registry, after checking there
// is room, and returns how much the volume grew and how long that took. The
// growth undercounts when the blobs were already stored, so the caller records
// repoSize instead when it can.
func mirror(ctx context.Context, e Env, o EnsureOptions, reg registry.Registry, addr, repo, tag string, m *metadata) (int64, time.Duration, error) {
	start := e.Now()
	used, total, err := e.Volume(ctx, o.Namespace)
	if err != nil {
		return 0, 0, err
	}
	est := estimate(m)
	free := total - used
	if total > 0 && free < est {
		return 0, 0, fmt.Errorf("the registry volume has %s free and mirroring %s needs about %s: run `vsno cache prune`, or make the volume bigger with `vsno setup --registry-size`",
			bytesText(free), o.Release.Requested, bytesText(est))
	}
	if total > 0 && free < est+est/4 {
		o.line(fmt.Sprintf("the registry has %s free: mirroring %s needs about %s", bytesText(free), o.Release.Requested, bytesText(est)))
	}
	creds, err := credentials(ctx, e, reg, addr)
	if err != nil {
		return 0, 0, err
	}
	files, err := builder.Tar(creds)
	if err != nil {
		return 0, 0, err
	}
	stop := e.reportProgress(ctx, o, used, est, start)
	err = runMirror(ctx, e, o, files, addr, repo, tag)
	stop()
	elapsed := e.Now().Sub(start)
	if err != nil {
		return 0, elapsed, err
	}
	if after, _, err := e.Volume(ctx, o.Namespace); err == nil && after > used {
		return after - used, elapsed, nil
	}
	return 0, elapsed, nil
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

// runMirror runs mirror.sh, passing its output lines on as they arrive.
func runMirror(ctx context.Context, e Env, o EnsureOptions, files []byte, host, repo, tag string) error {
	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		err := e.Builder.Run(ctx, script("mirror.sh"), []string{o.Release.Image, host, repo, tag},
			builder.IO{Stdin: bytes.NewReader(files), Stdout: pw, Stderr: &stderr})
		_ = pw.CloseWithError(err)
		done <- err
	}()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		o.line(sc.Text())
	}
	// A scanner that gives up would otherwise leave the writer blocked in the
	// pipe and this call waiting on it forever.
	_ = pr.Close()
	errRun := <-done
	if errRun != nil {
		return fmt.Errorf("mirroring %s: %w%s", o.Release.Requested, errRun, builder.Tail(stderr.String()))
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading the mirror's output: %w", err)
	}
	return nil
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
