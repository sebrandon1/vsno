package release

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebrandon1/go-toolbox/httpx"
)

// server fakes the update graph (stable-4.20 only) and the release controller (4.22 only).
func server(t *testing.T) Resolver {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/graph", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") != "stable-4.20" || r.URL.Query().Get("arch") != "amd64" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"nodes":[{"version":"4.20.9"},{"version":"4.20.46"},{"version":"4.20.10"},{"version":"4.19.30"},{"version":"4.20.0-rc.1"}]}`)
	})
	mux.HandleFunc("/streams/4.22.0-0.nightly/latest", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"name":"4.22.0-0.nightly-2026-11-10-072952","pullSpec":"registry.ci.openshift.org/ocp/release:4.22.0-0.nightly-2026-11-10-072952"}`)
	})
	mux.HandleFunc("/streams/4.23.0-0.nightly/latest", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{}`) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return Resolver{HTTP: srv.Client(), GraphURL: srv.URL + "/graph", NightlyURL: srv.URL + "/streams"}
}

func TestResolve(t *testing.T) {
	r := server(t)
	for _, c := range []struct {
		in   string
		want Release
	}{
		{"4.20", Release{Version: "4.20.46", Image: "quay.io/openshift-release-dev/ocp-release:4.20.46-x86_64"}},
		{"4.20.10", Release{Version: "4.20.10", Image: "quay.io/openshift-release-dev/ocp-release:4.20.10-x86_64"}},
		{"4.22-nightly", Release{Version: "4.22.0-0.nightly-2026-11-10-072952", Image: "registry.ci.openshift.org/ocp/release:4.22.0-0.nightly-2026-11-10-072952", Nightly: true}},
		{"4.22.0-0.nightly-2026-11-01-000000", Release{Version: "4.22.0-0.nightly-2026-11-01-000000", Image: "registry.ci.openshift.org/ocp/release:4.22.0-0.nightly-2026-11-01-000000", Nightly: true}},
		{"quay.io/example/release@sha256:abc", Release{Image: "quay.io/example/release@sha256:abc"}},
		{"registry.ci.openshift.org/ocp/release:4.22.0-ec.1", Release{Image: "registry.ci.openshift.org/ocp/release:4.22.0-ec.1", Nightly: true}},
	} {
		t.Run(c.in, func(t *testing.T) {
			got, err := r.Resolve(context.Background(), c.in)
			c.want.Requested = c.in
			if err != nil || got != c.want {
				t.Errorf("Resolve(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
			}
		})
	}
}

func TestResolveErrors(t *testing.T) {
	r := server(t)
	for in, want := range map[string]string{
		"4.20.99":      "not in channel stable-4.20",
		"4.21":         "reading channel stable-4.21",
		"4.21.3":       "reading channel stable-4.21",
		"4.23-nightly": "no accepted 4.23.0-0.nightly",
		"4.24-nightly": "finding the latest 4.24.0-0.nightly",
		"latest":       "unrecognized --version",
		"4":            "unrecognized --version",
	} {
		if _, err := r.Resolve(context.Background(), in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Resolve(%q) err = %v, want %q", in, err, want)
		}
	}
}

func TestResolveEmptyChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"nodes":[]}`) }))
	defer srv.Close()
	r := Resolver{GraphURL: srv.URL}
	if _, err := r.Resolve(context.Background(), "4.20"); err == nil || !strings.Contains(err.Error(), "no releases in channel stable-4.20") {
		t.Errorf("err = %v", err)
	}
}

func TestDefaults(t *testing.T) {
	var r Resolver
	if r.graphURL() != DefaultGraphURL || r.nightlyURL() != DefaultNightlyURL {
		t.Error("empty Resolver must use the public endpoints")
	}
}

func TestMinor(t *testing.T) {
	for v, want := range map[string]string{"4.21.5": "4.21", "4.22.0-0.nightly-2026-11-03": "4.22", "5": "", "": ""} {
		if got := Minor(v); got != want {
			t.Errorf("Minor(%q) = %q, want %q", v, got, want)
		}
	}
}

// A failed request names the URL and keeps httpx's typed error.
func TestGetErrors(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		status int
		is     func(error) bool
		want   string
	}{
		{http.StatusInternalServerError, httpx.IsHTTPError, "http error 500"},
		{http.StatusNotFound, httpx.IsNotFound, "not found"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(c.status) }))
		r := Resolver{HTTP: srv.Client(), GraphURL: srv.URL + "/graph"}
		var out struct{}
		err := r.get(ctx, srv.URL+"/graph", &out)
		srv.Close()
		if err == nil || !c.is(err) || !strings.Contains(err.Error(), srv.URL+"/graph") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("HTTP %d: %v", c.status, err)
		}
	}
	// Nothing listening: a network error, still naming the URL.
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL + "/graph"
	srv.Close()
	if err := (Resolver{}).get(ctx, u, &struct{}{}); err == nil || !httpx.IsNetworkError(err) || !strings.Contains(err.Error(), u) {
		t.Errorf("closed server: %v", err)
	}
}

// A malformed URL fails before any request, and a body past maxResponse is
// refused rather than read whole.
func TestGetBadURLAndOversizeBody(t *testing.T) {
	ctx := context.Background()
	if err := (Resolver{}).get(ctx, "http://bad host/graph", &struct{}{}); err == nil {
		t.Error("bad URL: want an error")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"nodes":[`))
		_, _ = w.Write(bytes.Repeat([]byte(" "), maxResponse))
		_, _ = w.Write([]byte(`]}`))
	}))
	defer srv.Close()
	var out struct{}
	if err := (Resolver{HTTP: srv.Client()}).get(ctx, srv.URL+"/graph", &out); err == nil || !strings.Contains(err.Error(), srv.URL+"/graph") {
		t.Errorf("oversize body: %v", err)
	}
}
