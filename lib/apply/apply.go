// Package apply server-side-applies rendered manifests and reports which
// objects actually changed, so reruns of vsno setup can show they are no-ops.
package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// FieldManager is the server-side apply field manager vsno uses.
const FieldManager = "vsno"

// Action says what applying an object did.
type Action string

const (
	Created   Action = "created"
	Updated   Action = "updated"
	Unchanged Action = "unchanged"

	// Plan actions, for a dry run.

	// Create means the object doesn't exist yet.
	Create Action = "create"
	// Reapply means vsno applied the object before; usually nothing changes.
	Reapply Action = "reapply"
	// TakeOver means the object exists but vsno didn't create it: applying
	// would overwrite the fields vsno sets.
	TakeOver Action = "take over"
)

// Change is the result of applying one object.
type Change struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Action    Action `json:"action"`
}

// Ref is "namespace/Kind/name", or "Kind/name" for cluster-scoped objects.
func (c Change) Ref() string {
	if c.Namespace == "" {
		return c.Kind + "/" + c.Name
	}
	return c.Namespace + "/" + c.Kind + "/" + c.Name
}

func (c Change) String() string { return c.Ref() + " " + string(c.Action) }

// Applier applies objects to a cluster.
type Applier interface {
	Apply(ctx context.Context, objs []*unstructured.Unstructured) ([]Change, error)
}

// Planner says what Apply would do, without changing anything.
type Planner interface {
	Plan(ctx context.Context, objs []*unstructured.Unstructured) ([]Change, error)
}

// Mapper is the subset of a RESTMapper Apply needs, plus Reset so kinds from
// newly installed CRDs (e.g. HyperConverged) are found.
type Mapper interface {
	RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error)
	Reset()
}

// SSA applies objects with server-side apply.
type SSA struct {
	Dyn    dynamic.Interface
	Mapper Mapper
}

// NewSSA builds an SSA applier with a cached discovery RESTMapper.
func NewSSA(dc discovery.DiscoveryInterface, dyn dynamic.Interface) *SSA {
	return &SSA{Dyn: dyn, Mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))}
}

// Apply implements Applier. It stops at the first error.
func (s *SSA) Apply(ctx context.Context, objs []*unstructured.Unstructured) ([]Change, error) {
	changes := make([]Change, 0, len(objs))
	for _, o := range objs {
		c, err := s.applyOne(ctx, o)
		if err != nil {
			return changes, err
		}
		changes = append(changes, c)
	}
	return changes, nil
}

func (s *SSA) resource(o *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := o.GroupVersionKind()
	m, err := s.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		s.Mapper.Reset()
		m, err = s.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gvk.Kind, err)
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		return s.Dyn.Resource(m.Resource).Namespace(o.GetNamespace()), nil
	}
	return s.Dyn.Resource(m.Resource), nil
}

func (s *SSA) applyOne(ctx context.Context, o *unstructured.Unstructured) (Change, error) {
	c := Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()}
	ri, err := s.resource(o)
	if err != nil {
		return c, err
	}
	before := ""
	cur, err := ri.Get(ctx, o.GetName(), metav1.GetOptions{})
	switch {
	case err == nil:
		before = cur.GetResourceVersion()
	case !apierrors.IsNotFound(err):
		return c, fmt.Errorf("getting %s: %w", c.Ref(), err)
	}
	got, err := ri.Apply(ctx, o.GetName(), o, metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	if err != nil {
		return c, fmt.Errorf("applying %s: %w", c.Ref(), err)
	}
	switch {
	case before == "":
		c.Action = Created
	case got.GetResourceVersion() != before:
		c.Action = Updated
	default:
		c.Action = Unchanged
	}
	return c, nil
}

// Plan implements Planner. It only reads: an object is Create when it or its
// kind (a CRD an earlier step installs) doesn't exist, Reapply when vsno's
// field manager already owns fields in it, and TakeOver otherwise.
func (s *SSA) Plan(ctx context.Context, objs []*unstructured.Unstructured) ([]Change, error) {
	changes := make([]Change, 0, len(objs))
	for _, o := range objs {
		c := Change{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), Action: Create}
		ri, err := s.resource(o)
		if err != nil {
			if !isNoMatch(err) {
				return changes, err
			}
			changes = append(changes, c)
			continue
		}
		cur, err := ri.Get(ctx, o.GetName(), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return changes, fmt.Errorf("getting %s: %w", c.Ref(), err)
		case managedBy(cur, FieldManager):
			c.Action = Reapply
		default:
			c.Action = TakeOver
		}
		changes = append(changes, c)
	}
	return changes, nil
}

func isNoMatch(err error) bool {
	var kind *meta.NoKindMatchError
	var res *meta.NoResourceMatchError
	return errors.As(err, &kind) || errors.As(err, &res)
}

func managedBy(o *unstructured.Unstructured, manager string) bool {
	for _, f := range o.GetManagedFields() {
		if f.Manager == manager {
			return true
		}
	}
	return false
}

// Label names an action in a Tally, e.g. {Create, "to create"}.
type Label struct {
	Action Action
	Text   string
}

// Tally is "<n> <text>, ..." for each labeled action that occurs in changes,
// in order, or none when no labeled action occurs.
func Tally(changes []Change, none string, labels ...Label) string {
	var parts []string
	for _, l := range labels {
		if n := Count(changes, l.Action); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, l.Text))
		}
	}
	if len(parts) == 0 {
		return none
	}
	return strings.Join(parts, ", ")
}

// Count returns how many changes have the given action.
func Count(changes []Change, a Action) int {
	n := 0
	for _, c := range changes {
		if c.Action == a {
			n++
		}
	}
	return n
}
