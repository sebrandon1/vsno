// Package release turns the --version a user types into an OpenShift release
// image: a released version, a minor (its latest stable z-stream), a nightly,
// the latest nightly of a minor, or a release image pullspec.
package release

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebrandon1/go-toolbox/httpx"
)

// Public endpoints; Resolver fields override them in tests.
const (
	DefaultGraphURL   = "https://api.openshift.com/api/upgrades_info/v1/graph"
	DefaultNightlyURL = "https://amd64.ocp.releases.ci.openshift.org/api/v1/releasestream"
	releaseRepo       = "quay.io/openshift-release-dev/ocp-release"
	nightlyRepo       = "registry.ci.openshift.org/ocp/release"
	// CIRegistry hosts nightlies; pulling them needs credentials for it.
	CIRegistry = "registry.ci.openshift.org"
)

// Release is a resolved --version.
type Release struct {
	// Requested is what the user typed.
	Requested string `json:"requested"`
	// Version is the OpenShift version, or "" for a pullspec (known after install).
	Version string `json:"version,omitempty"`
	Image   string `json:"image"`
	Nightly bool   `json:"nightly,omitempty"`
}

// Resolver looks versions up in the OpenShift update graph and the CI release
// controller.
type Resolver struct {
	HTTP       *http.Client
	GraphURL   string
	NightlyURL string
}

var (
	reVersion      = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
	reMinor        = regexp.MustCompile(`^(\d+)\.(\d+)$`)
	reLatestNightl = regexp.MustCompile(`^(\d+\.\d+)-nightly$`)
	reNightly      = regexp.MustCompile(`^\d+\.\d+\.\d+-0\.nightly-\d{4}-\d{2}-\d{2}-\d{6}$`)
)

// Forms lists the accepted --version forms, for help and errors.
const Forms = "4.20.46 (a release), 4.20 (its latest stable z-stream), 4.22-nightly (the latest accepted nightly), " +
	"4.22.0-0.nightly-2026-11-10-072952 (a nightly) or a release image pullspec"

// Resolve turns v into a release image.
func (r Resolver) Resolve(ctx context.Context, v string) (Release, error) {
	rel := Release{Requested: v}
	switch {
	case strings.ContainsAny(v, "/@"):
		rel.Image = v
		rel.Nightly = strings.HasPrefix(v, CIRegistry+"/")
	case reNightly.MatchString(v):
		rel.Version, rel.Image, rel.Nightly = v, nightlyRepo+":"+v, true
	case reLatestNightl.MatchString(v):
		name, pullSpec, err := r.latestNightly(ctx, reLatestNightl.FindStringSubmatch(v)[1])
		if err != nil {
			return rel, err
		}
		rel.Version, rel.Image, rel.Nightly = name, pullSpec, true
	case reMinor.MatchString(v):
		versions, err := r.Stable(ctx, v)
		if err != nil {
			return rel, err
		}
		if len(versions) == 0 {
			return rel, fmt.Errorf("no releases in channel stable-%s", v)
		}
		rel.Version = versions[len(versions)-1]
		rel.Image = releaseImage(rel.Version)
	case reVersion.MatchString(v):
		m := reVersion.FindStringSubmatch(v)
		versions, err := r.Stable(ctx, m[1]+"."+m[2])
		if err != nil {
			return rel, err
		}
		if !slices.Contains(versions, v) {
			return rel, fmt.Errorf("%s is not in channel stable-%s.%s; pass its release image pullspec to install it anyway", v, m[1], m[2])
		}
		rel.Version, rel.Image = v, releaseImage(v)
	default:
		return rel, fmt.Errorf("unrecognized --version %q; use %s", v, Forms)
	}
	return rel, nil
}

// Minor is "4.21" of "4.21.5" or "4.21.0-0.nightly-...", or "" when v has
// no minor.
func Minor(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

func releaseImage(v string) string { return releaseRepo + ":" + v + "-x86_64" }

// stable returns the versions in channel stable-<minor>, oldest first.
// Stable lists minor's releases in its stable channel, oldest first.
func (r Resolver) Stable(ctx context.Context, minor string) ([]string, error) {
	u := r.graphURL() + "?" + url.Values{"channel": {"stable-" + minor}, "arch": {"amd64"}}.Encode()
	var g struct {
		Nodes []struct {
			Version string `json:"version"`
		} `json:"nodes"`
	}
	if err := r.get(ctx, u, &g); err != nil {
		return nil, fmt.Errorf("reading channel stable-%s: %w", minor, err)
	}
	var out []string
	for _, n := range g.Nodes {
		if reVersion.MatchString(n.Version) {
			out = append(out, n.Version)
		}
	}
	slices.SortFunc(out, Compare)
	return out, nil
}

// latestNightly returns the latest accepted nightly of minor.
func (r Resolver) latestNightly(ctx context.Context, minor string) (name, pullSpec string, err error) {
	var l struct {
		Name     string `json:"name"`
		PullSpec string `json:"pullSpec"`
	}
	stream := minor + ".0-0.nightly"
	if err := r.get(ctx, r.nightlyURL()+"/"+stream+"/latest", &l); err != nil {
		return "", "", fmt.Errorf("finding the latest %s: %w", stream, err)
	}
	if l.Name == "" || l.PullSpec == "" {
		return "", "", fmt.Errorf("no accepted %s release", stream)
	}
	return l.Name, l.PullSpec, nil
}

// maxResponse bounds what get reads: a channel's update graph is a few MiB.
const maxResponse = 64 << 20

func (r Resolver) get(ctx context.Context, u string, into any) error {
	req, err := httpx.NewRequest(ctx, http.MethodGet, u)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	c := r.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req) //nolint:bodyclose // httpx.HandleResponse closes it
	if err != nil {
		return fmt.Errorf("%s: %w", u, httpx.AsNetworkError(err))
	}
	// Closes the body, turns a non-2xx status into a typed error, and decodes.
	if err := httpx.HandleResponse(resp, into, maxResponse); err != nil {
		return fmt.Errorf("%s: %w", u, err)
	}
	return nil
}

func (r Resolver) graphURL() string {
	if r.GraphURL != "" {
		return r.GraphURL
	}
	return DefaultGraphURL
}

func (r Resolver) nightlyURL() string {
	if r.NightlyURL != "" {
		return r.NightlyURL
	}
	return DefaultNightlyURL
}

// Compare orders x.y.z versions numerically: negative when a is older.
func Compare(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(pa), len(pb)) {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y
		}
	}
	return len(pa) - len(pb)
}
