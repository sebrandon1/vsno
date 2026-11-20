package cache

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
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
	digest, sources, err := describe(ctx, e, o.Release.Image)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("release %s lists no images to mirror", o.Release.Image)
	}
	addr, err := registry.Address(ctx, e.Kube, o.Namespace)
	if err != nil {
		return nil, err
	}
	repo := RepoPrefix + short(digest)
	// The release image itself is left pointing upstream on purpose: its
	// signature is fetched from the registry the pull resolves to
	// (use-sigstore-attachments), and a plain registry has none, so rewriting
	// it stalls the install at the cluster-version-operator. Its payload,
	// which is the 20 GB, is mapped below.
	if r, ok := repoOf(o.Release.Image); ok {
		sources = slices.DeleteFunc(sources, func(s string) bool { return s == r })
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
		ent = *prev // keep what it was mirrored as, not what this request calls it
	} else {
		start := e.Now()
		ent = Entry{Version: o.Release.Version, Image: o.Release.Image, Digest: digest, Repo: repo,
			Mirrored: start, Started: start, State: StateMirroring,
			Requested: appendUnique(oldRequested(prev), o.Release.Requested)}
		// Say what is happening before the long part starts: a create that is
		// mirroring should be visible in `vsno cache list`, not only in its log.
		if err := saveEntry(ctx, e, o.Namespace, digest, ent); err != nil {
			return nil, err
		}
		if ent.SizeBytes, elapsed, err = mirror(ctx, e, o, reg, addr, repo, short(digest), m); err != nil {
			ent.State, ent.Started = StateFailed, time.Time{}
			_ = saveEntry(ctx, e, o.Namespace, digest, ent) // best effort: the release is not usable
			return nil, err
		}
		ent.State, ent.Started = "", time.Time{}
	}
	ent.LastUsed = e.Now()
	ent.Requested = appendUnique(oldRequested(m.Entries[digest]), o.Release.Requested)
	if err := saveEntry(ctx, e, o.Namespace, digest, ent); err != nil {
		return nil, err
	}
	ps, err := pullSecret(ctx, e.Kube)
	if err != nil {
		return nil, err
	}
	merged, err := registry.AddAuth(ps, addr, reg.User, reg.Password)
	if err != nil {
		return nil, err
	}
	p := &Prepared{Hit: hit, Entry: ent, PullSecret: merged, TrustBundle: reg.CA, Elapsed: elapsed}
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

// mirror copies the release's payload into the registry, after checking there
// is room, and returns how much it took and how long that took.
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
	ps, err := pullSecret(ctx, e.Kube)
	if err != nil {
		return 0, 0, err
	}
	auth, err := registry.AddAuth(ps, addr, reg.User, reg.Password)
	if err != nil {
		return 0, 0, err
	}
	files, err := builder.Tar(map[string]string{"auth.json": auth, "ca.crt": reg.CA})
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
		return fmt.Errorf("mirroring %s: %w%s", o.Release.Requested, errRun, tail(stderr.String()))
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
