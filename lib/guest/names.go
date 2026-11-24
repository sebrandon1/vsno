package guest

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"math/big"
	"slices"

	"github.com/sebrandon1/vsno/lib/cluster"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Words for generated names, like Docker's: an adjective and a noun from
// nature. All short, so api-int.<name>.vsno.lab stays readable.
var (
	adjectives = []string{
		"agile", "amber", "bold", "brave", "bright", "calm", "clever", "cosmic", "crisp", "daring",
		"dusty", "eager", "fancy", "fierce", "gentle", "glad", "golden", "happy", "hidden", "jolly",
		"keen", "kind", "lively", "lucky", "mellow", "misty", "nimble", "noble", "quiet", "rapid",
		"rustic", "shiny", "silent", "silver", "snowy", "sunny", "swift", "tidy", "vivid", "witty",
	}
	nouns = []string{
		"aspen", "badger", "beacon", "bison", "breeze", "canyon", "cedar", "comet", "coral", "crane",
		"delta", "falcon", "fern", "fjord", "fox", "glacier", "harbor", "heron", "island", "lark",
		"lynx", "maple", "meadow", "moose", "nebula", "otter", "owl", "panda", "pebble", "pine",
		"puffin", "raven", "river", "sparrow", "summit", "tiger", "tundra", "walrus", "willow", "yak",
	}
)

func pick(words []string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return "", err
	}
	return words[n.Int64()], nil
}

// RandomName returns an adjective-noun name, such as brave-otter.
func RandomName() (string, error) {
	a, err := pick(adjectives)
	if err != nil {
		return "", err
	}
	n, err := pick(nouns)
	if err != nil {
		return "", err
	}
	return a + "-" + n, nil
}

// FreeName returns a random name no vSNO in ns has yet.
func FreeName(ctx context.Context, e Env, ns string) (string, error) {
	for range 20 {
		name, err := RandomName()
		if err != nil {
			return "", err
		}
		_, err = e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return name, nil
		case err != nil:
			return "", fmt.Errorf("checking for vSNO %s: %w", name, err)
		}
	}
	return "", fmt.Errorf("no free random name after 20 tries; pass a name")
}

// Names returns the vSNO names in ns: the VMs, plus any Service create left
// behind while it builds the ISO. It reads no guest through the builder, so it
// is cheap enough for shell completion.
func Names(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, ns string) ([]string, error) {
	set := map[string]struct{}{}
	vms, err := dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// No KubeVirt: the Services are all there can be.
	case err != nil:
		return nil, fmt.Errorf("listing vSNOs: %w", err)
	default:
		for _, vm := range vms.Items {
			set[vm.GetName()] = struct{}{}
		}
	}
	svcs, err := kube.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelName})
	if err != nil {
		return nil, fmt.Errorf("listing vSNO Services: %w", err)
	}
	for _, s := range svcs.Items {
		if name := s.Labels[LabelName]; name != "" {
			set[name] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(set)), nil
}
