package seed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/state"
)

// Image is one seed image in the environment's registry.
type Image struct {
	Ref       string    `json:"ref"`
	From      string    `json:"from"` // the vSNO it was made from (its repository name)
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest"`
	Version   string    `json:"version,omitempty"` // the seed's OpenShift version, from its label
	Format    string    `json:"format,omitempty"`  // LCA's seed format version
	SizeBytes int64     `json:"sizeBytes"`
	Created   time.Time `json:"created"` // the image's own build time
	// SyncedAt is when a `seed sync --from` brought the image in, if it did:
	// the copy keeps the upstream's build time, so this is what says how new
	// the seed is here.
	SyncedAt *time.Time `json:"syncedAt,omitempty"`
}

// inspected is one line of registry.sh list.
type inspected struct {
	Repo    string            `json:"repo"`
	Tag     string            `json:"tag"`
	Digest  string            `json:"digest"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels"`
	Size    int64             `json:"size"`
}

// parseImages reads registry.sh list's output.
func parseImages(host string, out []byte) ([]Image, error) {
	var imgs []Image
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var in inspected
		if err := json.Unmarshal(line, &in); err != nil {
			return nil, fmt.Errorf("reading the registry listing: %w", err)
		}
		img := Image{Ref: host + "/" + in.Repo + ":" + in.Tag, From: strings.TrimPrefix(in.Repo, RepoPrefix), Tag: in.Tag,
			Digest: in.Digest, Format: in.Labels[LabelFormat], SizeBytes: in.Size, Created: in.Created}
		var info struct {
			Version string `json:"seed_cluster_ocp_version"`
		}
		if json.Unmarshal([]byte(in.Labels[LabelClusterInfo]), &info) == nil {
			img.Version = info.Version
		}
		imgs = append(imgs, img)
	}
	slices.SortFunc(imgs, func(a, b Image) int { return strings.Compare(a.Ref, b.Ref) })
	return imgs, sc.Err()
}

// List returns the seed images in the environment's registry, from the builder.
func List(ctx context.Context, e Env, ns string) ([]Image, error) {
	reg, err := LoadRegistry(ctx, e.Kube, ns)
	if err != nil {
		return nil, fmt.Errorf("%w; `vsno status` says whether the environment's registry is up", err)
	}
	out, err := reg.Run(ctx, e.Builder, reg.Host, "list", RepoPrefix)
	if err != nil {
		return nil, fmt.Errorf("listing the registry through the builder: %w; `vsno status` says whether the registry and the builder are up", err)
	}
	imgs, err := parseImages(reg.Host, out)
	if err != nil {
		return nil, err
	}
	synced(ctx, e, ns, imgs) // an unreadable record only costs the sync time
	return imgs, nil
}

// synced fills each image's SyncedAt from the environment's state. Failing to
// read it is not a failure to list: the image's own Created still says
// something.
func synced(ctx context.Context, e Env, ns string, imgs []Image) {
	st, _, err := state.Load(ctx, e.Kube, ns)
	if err != nil || st == nil {
		return
	}
	for i := range imgs {
		if s, ok := st.Synced(imgs[i].Ref); ok && !s.SyncedAt.IsZero() {
			at := s.SyncedAt
			imgs[i].SyncedAt = &at
		}
	}
}

// recordSync notes in the environment's state when ref arrived.
func recordSync(ctx context.Context, e Env, ns, ref, digest string, at time.Time) error {
	st, _, err := state.Load(ctx, e.Kube, ns)
	if err != nil {
		return err
	}
	st.RecordSync(state.Sync{Ref: ref, Digest: digest, SyncedAt: at})
	return state.Save(ctx, e.Kube, ns, st)
}

// dropSync forgets the arrival times of images that are gone.
func dropSync(ctx context.Context, e Env, ns string, refs []string) error {
	st, _, err := state.Load(ctx, e.Kube, ns)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		st.DropSync(ref)
	}
	return state.Save(ctx, e.Kube, ns, st)
}

// Match returns the images arg names: a full reference, <vSNO>:<tag>, or
// <vSNO> for all of its seeds.
func Match(imgs []Image, arg string) []Image {
	var out []Image
	for _, img := range imgs {
		if arg == img.Ref || arg == img.From+":"+img.Tag || arg == RepoPrefix+img.From+":"+img.Tag || arg == img.From {
			out = append(out, img)
		}
	}
	return out
}

// selectImages returns the images args name, each once, failing on an arg
// that names none.
func selectImages(all []Image, args []string, ns string) ([]Image, error) {
	var out []Image
	for _, arg := range args {
		m := Match(all, arg)
		if len(m) == 0 {
			return nil, fmt.Errorf("no seed image %s in %s (see `vsno seed list`)", arg, ns)
		}
		for _, img := range m {
			if !slices.ContainsFunc(out, func(i Image) bool { return i.Ref == img.Ref }) {
				out = append(out, img)
			}
		}
	}
	return out, nil
}

// DeleteOptions are the user's choices for vsno seed delete.
type DeleteOptions struct {
	Namespace string
	Args      []string
	DryRun    bool
	Yes       bool
	// Confirm asks the user to type answer to approve question. Nil never approves.
	Confirm func(question, answer string) bool
	// ShowPlan, if set, is called with the images right before Confirm.
	ShowPlan func([]Image)
}

// DeleteResult is what delete found and removed.
type DeleteResult struct {
	Images  []Image  `json:"images"`
	Deleted []string `json:"deleted"`
}

// Delete removes the named seed images' tags from the registry. Their space
// comes back only when the registry is garbage collected.
func Delete(ctx context.Context, e Env, o DeleteOptions) (*DeleteResult, error) {
	res := &DeleteResult{}
	reg, err := LoadRegistry(ctx, e.Kube, o.Namespace)
	if err != nil {
		return res, err
	}
	all, err := List(ctx, e, o.Namespace)
	if err != nil {
		return res, err
	}
	if res.Images, err = selectImages(all, o.Args, o.Namespace); err != nil {
		return res, err
	}
	if o.DryRun {
		return res, nil
	}
	if !o.Yes {
		if o.ShowPlan != nil {
			o.ShowPlan(res.Images)
		}
		answer := strings.Join(o.Args, " ")
		q := fmt.Sprintf("This deletes %d seed image(s). Type %q to continue", len(res.Images), answer)
		if o.Confirm == nil || !o.Confirm(q, answer) {
			return res, errors.New("not confirmed; rerun with --yes to skip the question")
		}
	}
	refs := make([]string, 0, len(res.Images))
	for _, img := range res.Images {
		refs = append(refs, RepoPrefix+img.From+":"+img.Tag)
	}
	out, err := reg.Run(ctx, e.Builder, append([]string{reg.Host, "delete"}, refs...)...)
	for _, line := range strings.Split(string(out), "\n") {
		if ref, ok := strings.CutPrefix(line, "deleted "); ok {
			res.Deleted = append(res.Deleted, reg.Host+"/"+ref)
		}
	}
	if err != nil {
		return res, fmt.Errorf("deleting from the registry: %w", err)
	}
	gone := make([]string, 0, len(res.Images))
	for _, img := range res.Images {
		gone = append(gone, img.Ref)
	}
	if err := dropSync(ctx, e, o.Namespace, gone); err != nil {
		return res, fmt.Errorf("the images were deleted, but forgetting when they arrived: %w", err)
	}
	return res, nil
}
