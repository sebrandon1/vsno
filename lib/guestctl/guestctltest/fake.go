// Package guestctltest is a fake guestctl.Access for the packages that run
// commands on a vSNO.
package guestctltest

import (
	"context"
	"io"
	"slices"
	"strings"

	"github.com/sebrandon1/vsno/lib/builder"
)

// Reply is one canned answer: what the command prints and how it ends.
type Reply struct {
	Out string
	Err error
}

// Fake answers oc and ssh by their arguments joined with spaces, without a
// trailing "-o json". Each key's replies are used in turn, the last one
// repeating; unknown keys print nothing and succeed.
type Fake struct {
	OcReplies, SSHReplies map[string][]Reply
	// Calls are "oc <args>" and "ssh <args>", in order.
	Calls []string
	// Stdin is what each call that had stdin was given.
	Stdin []string
}

func (f *Fake) answer(m map[string][]Reply, kind string, args []string, s builder.IO) error {
	key := strings.TrimSuffix(strings.Join(args, " "), " -o json")
	f.Calls = append(f.Calls, kind+" "+key)
	if s.Stdin != nil {
		b, _ := io.ReadAll(s.Stdin)
		f.Stdin = append(f.Stdin, string(b))
	}
	q := m[key]
	if len(q) == 0 {
		return nil
	}
	r := q[0]
	if len(q) > 1 {
		m[key] = q[1:]
	}
	if s.Stdout != nil {
		_, _ = io.WriteString(s.Stdout, r.Out)
	}
	return r.Err
}

// Oc implements guestctl.Access.
func (f *Fake) Oc(_ context.Context, args []string, s builder.IO) error {
	return f.answer(f.OcReplies, "oc", args, s)
}

// SSH implements guestctl.Access.
func (f *Fake) SSH(_ context.Context, args []string, s builder.IO) error {
	return f.answer(f.SSHReplies, "ssh", args, s)
}

// Ran reports whether a call starting with prefix was made.
func (f *Fake) Ran(prefix string) bool {
	return slices.ContainsFunc(f.Calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}
