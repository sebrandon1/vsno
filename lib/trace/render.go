package trace

import (
	"net/http"
	"net/url"
	"strings"
)

// RoundTripper traces each API request as kubectl would name it, then passes it
// to next.
func RoundTripper(next http.RoundTripper) http.RoundTripper { return traced{next: next} }

type traced struct{ next http.RoundTripper }

func (t traced) RoundTrip(req *http.Request) (*http.Response, error) {
	if Enabled() {
		if line, ok := Command(req.Method, req.URL); ok {
			Line("→ " + line)
		}
	}
	return t.next.RoundTrip(req)
}

// silent are the subresources whose request carries a command or a stream of
// its own, which the builder reports better than a bare "exec pods/x".
var silent = map[string]bool{
	"attach": true, "console": true, "exec": true, "portforward": true,
	"proxy": true, "vnc": true,
}

// Command names one API request the way kubectl would: "get pods -n vsno".
// ok is false for a request worth skipping.
func Command(method string, u *url.URL) (string, bool) {
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(p) >= 3 && p[0] == "api" && p[1] == "v1":
		return named(method, u, "", p[2:])
	case len(p) >= 4 && p[0] == "apis":
		// Kubernetes execs and screenshots report their group as
		// subresources.kubevirt.io; the resource belongs to kubevirt.io.
		return named(method, u, strings.TrimPrefix(p[1], "subresources."), p[3:])
	}
	return raw(method, u), true
}

// raw renders a path that names no resource, such as /version or /apis.
func raw(method string, u *url.URL) string { return strings.ToUpper(method) + " " + u.Path }

// named renders a resource path: "get virtualmachines.kubevirt.io seed -n vsno".
func named(method string, u *url.URL, group string, rest []string) (string, bool) {
	ns := ""
	if len(rest) > 2 && rest[0] == "namespaces" {
		ns, rest = rest[1], rest[2:]
	}
	if len(rest) == 0 {
		return raw(method, u), true
	}
	var name, sub string
	if len(rest) > 1 {
		name = rest[1]
	}
	if len(rest) > 2 {
		sub = rest[2]
	}
	if silent[sub] {
		return "", false
	}
	if sub == "log" {
		return logsLine(name, ns, u.Query()), true
	}
	verb := verbFor(method, name != "")
	var b strings.Builder
	b.WriteString(verb + " " + rest[0])
	if group != "" {
		b.WriteString("." + group)
	}
	if name != "" {
		b.WriteString(" " + name)
	}
	if ns != "" {
		b.WriteString(" -n " + ns)
	}
	if sub != "" {
		b.WriteString(" " + sub)
	}
	b.WriteString(queryFlags(u.Query()))
	return b.String(), true
}

// logsLine renders a pod log read, which names the pod rather than the
// resource it belongs to.
func logsLine(name, ns string, q url.Values) string {
	s := "logs " + name
	if ns != "" {
		s += " -n " + ns
	}
	return s + queryFlags(q)
}

// verbFor is the kubectl verb for an HTTP method.
func verbFor(method string, named bool) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "get"
	case http.MethodPost:
		return "create"
	case http.MethodPut:
		return "replace"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		if named {
			return "delete"
		}
		return "deletecollection"
	}
	return strings.ToLower(method)
}

// queryFlags renders the three query options kubectl has flags for. Everything
// else a client sends (resourceVersion, limits, timeouts) is left out.
func queryFlags(q url.Values) string {
	var b strings.Builder
	if v := q.Get("labelSelector"); v != "" {
		b.WriteString(" -l " + v)
	}
	if v := q.Get("fieldSelector"); v != "" {
		b.WriteString(" --field-selector " + v)
	}
	if w := q.Get("watch"); w == "true" || w == "1" {
		b.WriteString(" -w")
	}
	return b.String()
}
