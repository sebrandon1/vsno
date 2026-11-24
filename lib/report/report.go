// Package report gathers what a bug report or a demo wants in one place: the
// environment's status, its vSNOs, the release cache, the seed images and the
// builder's leftovers. It reads; it never writes. Nothing it holds carries
// credentials — the registry's login, the S3 keys and the pull secret are
// names only, never values.
package report

import (
	"context"
	"fmt"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/isos"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/status"
	"k8s.io/client-go/rest"
)

// Env is what collecting needs: the guest environment, and the rest config the
// release cache reads the registry's volume with.
type Env struct {
	guest.Env
	Config *rest.Config
}

// Report is one environment, as every read command reports it, together.
type Report struct {
	Namespace string         `json:"namespace"`
	Status    *status.Report `json:"status"`
	VSNOs     []guest.Guest  `json:"vsnos,omitempty"`
	Cache     *cache.Report  `json:"cache,omitempty"`
	Seeds     []seed.Image   `json:"seeds,omitempty"`
	ISOs      *isos.Report   `json:"isos,omitempty"`
	// Warnings are the parts that could not be read. A report with warnings is
	// still a report: the builder being down must not cost the rest.
	Warnings []string `json:"warnings,omitempty"`
}

// Collect reads every part. Only a namespace that cannot be read is an error;
// each other part that fails becomes a warning.
func Collect(ctx context.Context, e Env, ns string) (*Report, error) {
	st, err := status.Collect(ctx, e.Kube, e.Dyn, ns)
	if err != nil {
		return nil, err
	}
	rep := &Report{Namespace: ns, Status: st}
	warn := func(what string, err error) {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: %v", what, err))
	}
	if !st.Exists {
		return rep, nil
	}
	vsnos, err := guest.List(ctx, e.Env, ns)
	if err != nil {
		warn("the vSNOs", err)
	} else {
		rep.VSNOs = vsnos
	}
	cacheEnv := cache.Env{Kube: e.Kube, Dyn: e.Dyn, Config: e.Config, Builder: e.Builder}
	if c, err := cache.List(ctx, cacheEnv, ns, users(vsnos)); err != nil {
		warn("the release cache", err)
	} else {
		rep.Cache = c
	}
	if seeds, err := seed.List(ctx, seed.Env{Env: e.Env}, ns); err != nil {
		warn("the seed images", err)
	} else {
		rep.Seeds = seeds
	}
	if i, err := isos.List(ctx, isos.Env{Env: e.Env}, ns); err != nil {
		warn("the builder's files", err)
	} else {
		rep.ISOs = i
	}
	return rep, nil
}

// users maps a release image to the vSNOs running it, as `cache list` shows.
func users(vsnos []guest.Guest) map[string][]string {
	out := map[string][]string{}
	for _, g := range vsnos {
		if g.Cached && g.Image != "" {
			out[g.Image] = append(out[g.Image], g.Name)
		}
	}
	return out
}
