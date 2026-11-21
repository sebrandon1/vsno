// Package guestctl runs oc and ssh on a vSNO and reads what they report: the
// shared plumbing of the commands that work inside a guest (seed generation,
// IBU preparation and upgrades). It knows nothing about what those commands
// do; it reads objects, applies manifests, waits and reports progress.
package guestctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/precheck"
)

// Access runs oc and ssh on one vSNO.
type Access interface {
	Oc(ctx context.Context, args []string, s builder.IO) error
	SSH(ctx context.Context, args []string, s builder.IO) error
}

// ThroughBuilder reaches vSNO name in namespace ns through the builder pod,
// like vsno oc and vsno ssh.
func ThroughBuilder(e guest.Env, ns, name string) Access {
	return builderAccess{e: e, ns: ns, name: name}
}

type builderAccess struct {
	e        guest.Env
	ns, name string
}

func (b builderAccess) Oc(ctx context.Context, args []string, s builder.IO) error {
	return guest.Oc(ctx, b.e, b.ns, b.name, args, s)
}

func (b builderAccess) SSH(ctx context.Context, args []string, s builder.IO) error {
	return guest.SSH(ctx, b.e, b.ns, b.name, args, s)
}

// Get runs oc ... -o json on the vSNO into out. With --ignore-not-found in
// args, a missing object gives found == false.
func Get(ctx context.Context, a Access, out any, args ...string) (found bool, err error) {
	var stdout, stderr bytes.Buffer
	if err := a.Oc(ctx, append(args, "-o", "json"), builder.IO{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return false, ocErr(args, err, stderr.String())
	}
	if len(bytes.TrimSpace(stdout.Bytes())) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return false, fmt.Errorf("oc %s: parsing the output: %w", strings.Join(args, " "), err)
	}
	return true, nil
}

// Oc runs oc on the vSNO with stdin (none when ""), discarding its output.
func Oc(ctx context.Context, a Access, stdin string, args ...string) error {
	var stderr bytes.Buffer
	s := builder.IO{Stdout: &bytes.Buffer{}, Stderr: &stderr}
	if stdin != "" {
		s.Stdin = strings.NewReader(stdin)
	}
	if err := a.Oc(ctx, args, s); err != nil {
		return ocErr(args, err, stderr.String())
	}
	return nil
}

// Apply applies manifests on the vSNO.
func Apply(ctx context.Context, a Access, manifests string) error {
	return Oc(ctx, a, manifests, "apply", "-f", "-")
}

func ocErr(args []string, err error, stderr string) error {
	msg := fmt.Sprintf("oc %s: %v", strings.Join(args, " "), err)
	if s := strings.TrimSpace(stderr); s != "" {
		msg += ": " + s[strings.LastIndex(s, "\n")+1:]
	}
	return errors.New(msg)
}

// SSHOut runs a command on the vSNO's node and returns its trimmed output.
func SSHOut(ctx context.Context, a Access, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	err := a.SSH(ctx, args, builder.IO{Stdout: &stdout, Stderr: &stderr})
	return strings.TrimSpace(stdout.String()), err
}

// SSH runs a command on the vSNO's node, discarding its output.
func SSH(ctx context.Context, a Access, args ...string) error {
	_, err := SSHOut(ctx, a, args...)
	return err
}

// Condition is a status condition as oc prints it.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// Conditions are an object's status conditions.
type Conditions []Condition

// Find returns condition t, or nil.
func (cs Conditions) Find(t string) *Condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}

// IsTrue reports whether condition t is True.
func (cs Conditions) IsTrue(t string) bool {
	c := cs.Find(t)
	return c != nil && c.Status == "True"
}

// Object is the part of any object vsno reads: its name, phase and conditions.
type Object struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Phase      string     `json:"phase"`
		Conditions Conditions `json:"conditions"`
	} `json:"status"`
}

// Deployment is what vsno reads of a Deployment on a vSNO.
type Deployment struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas            int `json:"replicas"`
		UpdatedReplicas     int `json:"updatedReplicas"`
		AvailableReplicas   int `json:"availableReplicas"`
		UnavailableReplicas int `json:"unavailableReplicas"`
	} `json:"status"`
}

// Image is the image of the container named name, or "".
func (d Deployment) Image(name string) string {
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

// RunsImage reports whether any container runs image.
func (d Deployment) RunsImage(image string) bool {
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Image == image {
			return true
		}
	}
	return false
}

// RolledOut reports whether every replica is updated and available. The old
// replicas count as available until they go, so available alone doesn't say
// the new ones started.
func (d Deployment) RolledOut() bool {
	st := d.Status
	return st.UpdatedReplicas > 0 && st.UpdatedReplicas == st.Replicas && st.UnavailableReplicas == 0 && st.AvailableReplicas > 0
}

// ObjectList is what oc get prints for a kind.
type ObjectList struct {
	Items []Object `json:"items"`
}

// Cond returns condition t, or nil.
func (o Object) Cond(t string) *Condition { return o.Status.Conditions.Find(t) }

// IsTrue reports whether condition t is True.
func (o Object) IsTrue(t string) bool { return o.Status.Conditions.IsTrue(t) }

// ErrTimeout is what Poll returns when limit passes.
var ErrTimeout = errors.New("timed out")

// Clock is how Poll waits: every Every, reading the time from Now. Tests use
// a Now that jumps ahead, so limits pass without waiting.
type Clock struct {
	Every time.Duration
	Now   func() time.Time
}

// Poll calls fn every c.Every until it's done, fails, or limit passes.
func Poll(ctx context.Context, c Clock, limit time.Duration, fn func() (bool, error)) error {
	deadline := c.Now().Add(limit)
	for {
		done, err := fn()
		if done || err != nil {
			return err
		}
		if c.Now().After(deadline) {
			return ErrTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.Every):
		}
	}
}

// Step runs fn as one progress step, which shows its detail when it is done
// and its error when it fails.
func Step(p precheck.Progress, title string, fn func(precheck.ProgressStep) (string, error)) error {
	ps := p.Start(title)
	detail, err := fn(ps)
	if err != nil {
		ps.Fail(err.Error())
		return err
	}
	ps.Done(detail)
	return nil
}
