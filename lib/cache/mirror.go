package cache

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"

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
	ent := Entry{Version: o.Release.Version, Image: o.Release.Image, Digest: digest, Repo: repo}
	m, err := load(ctx, e.Kube, o.Namespace)
	if err != nil {
		return nil, err
	}
	hit := m.Entries[digest] != nil
	if hit {
		ent.Mirrored, ent.SizeBytes = m.Entries[digest].Mirrored, m.Entries[digest].SizeBytes
	} else {
		ent.Mirrored = e.Now()
		if ent.SizeBytes, err = mirror(ctx, e, o, reg, addr, repo, short(digest), m); err != nil {
			return nil, err
		}
	}
	ent.LastUsed = e.Now()
	ent.Requested = appendUnique(oldRequested(m.Entries[digest]), o.Release.Requested)
	if err := update(ctx, e.Kube, o.Namespace, func(m *metadata) { m.Entries[digest] = &ent }); err != nil {
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
	p := &Prepared{Hit: hit, Entry: ent, PullSecret: merged, TrustBundle: reg.CA}
	for _, s := range sources {
		p.Sources = append(p.Sources, ImageDigestSource{Source: s, Mirrors: []string{addr + "/" + repo}})
	}
	return p, nil
}

// mirror copies the release's payload into the registry, after checking there
// is room, and returns how much it took.
func mirror(ctx context.Context, e Env, o EnsureOptions, reg registry.Registry, addr, repo, tag string, m *metadata) (int64, error) {
	used, total, err := e.usage(ctx, o.Namespace)
	if err != nil {
		return 0, err
	}
	est := estimate(m)
	free := total - used
	if total > 0 && free < est {
		return 0, fmt.Errorf("the registry volume has %s free and mirroring %s needs about %s: run `vsno cache prune`, or make the volume bigger with `vsno setup --registry-size`",
			bytesText(free), o.Release.Requested, bytesText(est))
	}
	if total > 0 && free < est+est/4 {
		o.line(fmt.Sprintf("the registry has %s free: mirroring %s needs about %s", bytesText(free), o.Release.Requested, bytesText(est)))
	}
	ps, err := pullSecret(ctx, e.Kube)
	if err != nil {
		return 0, err
	}
	auth, err := registry.AddAuth(ps, addr, reg.User, reg.Password)
	if err != nil {
		return 0, err
	}
	files, err := builder.Tar(map[string]string{"auth.json": auth, "ca.crt": reg.CA})
	if err != nil {
		return 0, err
	}
	if err := runMirror(ctx, e, o, files, addr, repo, tag); err != nil {
		return 0, err
	}
	if after, _, err := e.usage(ctx, o.Namespace); err == nil && after > used {
		return after - used, nil
	}
	return 0, nil
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
	for sc.Scan() {
		o.line(sc.Text())
	}
	if err := <-done; err != nil {
		return fmt.Errorf("mirroring %s: %w%s", o.Release.Requested, err, tail(stderr.String()))
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
