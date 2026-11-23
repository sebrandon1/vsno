package seed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/registry"
)

// SyncOptions are the user's choices for vsno seed sync.
type SyncOptions struct {
	Namespace string
	Seed      string // <vsno>[:<version>], as `vsno seed list` shows it
	To        string // a repository to copy the seed to
	From      string // an image to copy the seed from
	AuthFile  string // credentials for the registry that is not the environment's
	CAPath    string // the far registry's CA certificate, PEM; "" uses the system trust store
	Insecure  bool   // that registry may be plain HTTP or untrusted
	Overwrite bool   // replace the far side's tag when it already exists
}

// SyncResult is one seed image copied.
type SyncResult struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Digest      string `json:"digest,omitempty"`
	SizeBytes   int64  `json:"sizeBytes"`
}

// Sync copies a seed image between the environment's registry and another one:
// --to pushes a seed out, --from brings an image in under the name and version
// the seed is given.
func Sync(ctx context.Context, e Env, o SyncOptions) (*SyncResult, error) {
	if err := o.checkDirection(); err != nil {
		return nil, err
	}
	reg, err := LoadRegistry(ctx, e.Kube, o.Namespace)
	if err != nil {
		return nil, err
	}
	if imageHost(o.To) == reg.Host && o.Insecure {
		return nil, fmt.Errorf("--insecure would weaken the trust in %s, whose CA this environment already has", reg.Host)
	}
	res, err := o.refs(ctx, e, reg)
	if err != nil {
		return nil, err
	}
	far := o.To
	if o.From != "" {
		far = o.From
	}
	auth, err := farAuth(o.AuthFile, far, o.From == "", precheck.DefaultAuthFiles())
	if err != nil {
		return nil, err
	}
	ca, err := readIfSet(o.CAPath, "CA")
	if err != nil {
		return nil, err
	}
	out, err := reg.Sync(ctx, e.Builder, registry.SyncOptions{
		Source: res.Source, Destination: res.Destination, AuthFile: auth, CAFile: ca,
		LocalSource: o.To != "", Insecure: o.Insecure, Overwrite: o.Overwrite,
	})
	if err != nil {
		return res, err
	}
	res.Digest, res.SizeBytes = out.Digest, out.SizeBytes
	if o.From != "" {
		// The copy arrived here. Its manifest keeps the upstream's build time,
		// so remember when it landed; seed list says so.
		at := time.Now()
		if e.Now != nil {
			at = e.Now()
		}
		if err := recordSync(ctx, e, o.Namespace, res.Destination, res.Digest, at); err != nil {
			return res, fmt.Errorf("the image was copied, but recording when: %w", err)
		}
	}
	return res, nil
}

// checkDirection refuses neither or both of --to and --from.
func (o SyncOptions) checkDirection() error {
	switch {
	case o.To == "" && o.From == "":
		return errors.New("pick a direction: --to <repo> to push the seed out, or --from <image> to bring one in")
	case o.To != "" && o.From != "":
		return errors.New("--to and --from copy in different directions; pass one")
	}
	return nil
}

// refs works out the two references: a pull's come from the arguments, and a
// push's from the registry's own listing.
func (o SyncOptions) refs(ctx context.Context, e Env, reg Registry) (*SyncResult, error) {
	name, version, err := splitSeed(o.Seed, o.From != "")
	if err != nil {
		return nil, err
	}
	res := &SyncResult{Name: name, Version: version}
	if o.From == "" {
		if err := pushRefs(ctx, e, o, res); err != nil {
			return nil, err
		}
		return res, nil
	}
	if imageHost(o.From) == reg.Host {
		return nil, fmt.Errorf("--from takes an image somewhere other than %s; a seed already here needs only --to", reg.Host)
	}
	res.Source, res.Destination = o.From, Ref(reg, name, version)
	return res, nil
}

// pushRefs fills in the refs of a push from the registry's own listing, so the
// tag and version come from the image rather than from the user's typing.
func pushRefs(ctx context.Context, e Env, o SyncOptions, res *SyncResult) error {
	all, err := List(ctx, e, o.Namespace)
	if err != nil {
		return err
	}
	imgs, err := selectImages(all, []string{o.Seed}, o.Namespace)
	if err != nil {
		return err
	}
	if len(imgs) > 1 {
		return fmt.Errorf("%s names %d seed images; add the version, as %s:%s", o.Seed, len(imgs), imgs[0].From, imgs[0].Tag)
	}
	img := imgs[0]
	res.Name, res.Source = img.From, img.Ref
	if img.Version != "" {
		res.Version = img.Version
	}
	res.Destination = strings.TrimSuffix(o.To, "/") + ":" + img.Tag
	return nil
}

// farAuth returns the far registry's credentials: the file the user named, or
// the first of files covering it. A push with none is an error, because no
// registry takes a push anonymously; a pull may be, since public repositories
// allow it.
func farAuth(file, ref string, push bool, files []string) (string, error) {
	if file == "" {
		f, _, err := precheck.FindRegistryAuth(ref, files)
		if err != nil {
			if !push {
				return "", nil
			}
			return "", fmt.Errorf("no credentials for %s; `podman login %s`, or pass --authfile", imageHost(ref), imageHost(ref))
		}
		file = f
	}
	b, err := os.ReadFile(file) // #nosec G304 -- the user's own auth file
	if err != nil {
		return "", fmt.Errorf("reading the auth file: %w", err)
	}
	return string(b), nil
}

// readIfSet returns the contents of path, or "" when the user set nothing.
func readIfSet(path, what string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the user's own file
	if err != nil {
		return "", fmt.Errorf("reading the %s: %w", what, err)
	}
	return string(b), nil
}

// splitSeed reads <vsno>[:<version>]. The version is what follows the last
// colon, so a registry port in a name cannot be mistaken for one.
func splitSeed(arg string, needVersion bool) (name, version string, err error) {
	name, version = arg, ""
	if i := strings.LastIndex(arg, ":"); i > strings.LastIndex(arg, "/") {
		name, version = arg[:i], arg[i+1:]
	}
	name = strings.TrimPrefix(name, RepoPrefix)
	if name == "" {
		return "", "", fmt.Errorf("name the seed as <vsno>[:<version>], not %q", arg)
	}
	if needVersion && version == "" {
		return "", "", fmt.Errorf("--from stores the image as <vsno>:<version>; name the version, as %s:<version>", name)
	}
	return name, version, nil
}
