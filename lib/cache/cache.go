// Package cache mirrors OpenShift release payloads into the environment's
// registry, so the second vSNO of a version installs from the cluster instead
// of pulling 15-20 GB from the internet.
package cache

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/registry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// ConfigMap records what is cached, like vsno-state records setup's.
	ConfigMap = "vsno-cache"
	// RepoPrefix is where releases are mirrored: ocp/release/<sha16>.
	RepoPrefix = "ocp/release/"
	// key is the ConfigMap's key.
	key = "cache.json"
)

// Env is what the cache needs: the builder to mirror and read payloads, and
// the cluster for the metadata ConfigMap and the registry's volume.
type Env struct {
	Kube    kubernetes.Interface
	Dyn     dynamic.Interface
	Config  *rest.Config
	Builder builder.Runner
	Now     func() time.Time
	// Usage reads the registry volume's used and total size; nil reads it from
	// the registry pod.
	Usage func(ctx context.Context, ns string) (used, total int64, err error)
	// Digests lists a repository's manifest digests; nil reads them from the
	// registry pod's volume.
	Digests func(ctx context.Context, ns, repo string) ([]string, error)
	// Poll is how often to wait for the registry and its Job; 0 = 2s.
	Poll time.Duration
	// Progress is how often a mirror reports its throughput; 0 = 5s.
	Progress time.Duration
}

// Volume is the registry volume's used and total size, however Env gets them.
func (e Env) Volume(ctx context.Context, ns string) (used, total int64, err error) {
	if e.Usage != nil {
		return e.Usage(ctx, ns)
	}
	return registry.Usage(ctx, e.Config, e.Kube, ns)
}

func (e *Env) defaults() {
	if e.Now == nil {
		e.Now = time.Now
	}
}

// Entry is one cached release.
type Entry struct {
	Version string `json:"version,omitempty"` // "" for a pullspec
	// Requested are the forms of the version users asked for.
	Requested []string `json:"requested,omitempty"`
	Image     string   `json:"image"`  // the release image that was mirrored
	Digest    string   `json:"digest"` // the payload's digest, the cache key
	Repo      string   `json:"repo"`   // ocp/release/<sha16>
	SizeBytes int64    `json:"sizeBytes,omitempty"`
	// Measured is true when SizeBytes was counted from the blobs the
	// repository references. Entries without it recorded how much the
	// registry volume grew during the mirror, which is too little when the
	// blobs were already there (a second mirror of a release), so they are
	// measured again on their next use.
	Measured bool      `json:"measured,omitempty"`
	Mirrored time.Time `json:"mirrored"`
	LastUsed time.Time `json:"lastUsed"`
	// Users are the vSNOs running this release, filled in by List.
	Users []string `json:"users,omitempty"`
	InUse bool     `json:"inUse,omitempty"`
	// State is where the entry is in its life. Empty means ready, which is
	// also what entries written before this field existed are.
	State State `json:"state,omitempty"`
	// Started is when the current mirror began; set only while mirroring.
	Started time.Time `json:"started,omitempty"`
}

// State is how far a cache entry has got.
type State string

const (
	// StateMirroring means a mirror is writing this release into the registry.
	StateMirroring State = "mirroring"
	// StateFailed means the last mirror failed; its repository may be partial.
	StateFailed State = "failed"
)

// StateText is what the commands show for the entry.
func (e Entry) StateText() string {
	switch e.State {
	case StateMirroring:
		return string(StateMirroring)
	case StateFailed:
		return string(StateFailed)
	}
	return "ready"
}

// Ref is the mirrored release image in host's registry.
func (e Entry) Ref(host string) string {
	return host + "/" + e.Repo + ":" + short(e.Digest)
}

// Report is what `vsno cache list` shows.
type Report struct {
	Entries []Entry `json:"entries"`
	// Used and Total are the registry volume's.
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
}

// List returns the cached releases, with the vSNOs that use each one. users
// maps a release image to the vSNOs running it.
func List(ctx context.Context, e Env, ns string, users map[string][]string) (*Report, error) {
	e.defaults()
	m, err := load(ctx, e.Kube, ns)
	if err != nil {
		return nil, err
	}
	rep := &Report{Entries: make([]Entry, 0, len(m.Entries))}
	for _, ent := range m.Entries {
		ent.Users = users[ent.Image]
		ent.InUse = len(ent.Users) > 0
		rep.Entries = append(rep.Entries, *ent)
	}
	slices.SortFunc(rep.Entries, func(a, b Entry) int {
		if c := strings.Compare(a.Version, b.Version); c != 0 {
			return c
		}
		return a.Mirrored.Compare(b.Mirrored)
	})
	rep.Used, rep.Total, _ = e.Volume(ctx, ns) // a missing pod leaves them zero
	return rep, nil
}

// short is a digest's first 16 hex digits: the repository each mirrored
// release lives in.
func short(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 16 {
		d = d[:16]
	}
	return d
}

// info is the part of `oc adm release info -o json` the cache reads.
type info struct {
	Digest     string `json:"digest"`
	References struct {
		Spec struct {
			Tags []struct {
				From struct {
					Name string `json:"name"`
				} `json:"from"`
			} `json:"tags"`
		} `json:"spec"`
	} `json:"references"`
}

// payload is what describe reads of a release.
type payload struct {
	Digest string
	// Sources are the repositories its images come from.
	Sources []string
	// Images are its component images, by digest.
	Images []string
}

// describe reads a release's digest, the repositories it pulls from and its
// component images.
func describe(ctx context.Context, e Env, image string) (payload, error) {
	var r payload
	ps, err := pullSecret(ctx, e.Kube)
	if err != nil {
		return r, err
	}
	var stdout, stderr bytes.Buffer
	err = e.Builder.Run(ctx, script("release-info.sh"), []string{image},
		builder.IO{Stdin: strings.NewReader(ps), Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return r, fmt.Errorf("reading release %s: %w%s", image, err, builder.Tail(stderr.String()))
	}
	var in info
	if err := json.Unmarshal(stdout.Bytes(), &in); err != nil {
		return r, fmt.Errorf("reading release %s: %w", image, err)
	}
	if in.Digest == "" {
		return r, fmt.Errorf("release %s has no digest", image)
	}
	r.Digest = in.Digest
	for _, t := range in.References.Spec.Tags {
		if name, ok := repoOf(t.From.Name); ok && !slices.Contains(r.Sources, name) {
			r.Sources = append(r.Sources, name)
		}
		if strings.Contains(t.From.Name, "@sha256:") && !slices.Contains(r.Images, t.From.Name) {
			r.Images = append(r.Images, t.From.Name)
		}
	}
	slices.Sort(r.Sources)
	slices.Sort(r.Images)
	return r, nil
}

// repoOf strips an image reference's tag or digest, leaving its repository.
func repoOf(ref string) (string, bool) {
	if ref == "" || !strings.Contains(ref, "/") {
		return "", false
	}
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref, true
}

// pullSecret is the cluster's pull secret: what a mirror needs for quay.io and,
// for a nightly, registry.ci.openshift.org.
func pullSecret(ctx context.Context, kube kubernetes.Interface) (string, error) {
	s, err := kube.CoreV1().Secrets("openshift-config").Get(ctx, "pull-secret", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the cluster pull secret: %w", err)
	}
	return string(s.Data[corev1.DockerConfigJsonKey]), nil
}

//go:embed scripts/*.sh
var files embed.FS

func script(name string) string {
	b, err := files.ReadFile("scripts/" + name)
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(b)
}
