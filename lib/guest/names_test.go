package guest

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func TestWordsMakeValidNames(t *testing.T) {
	for _, a := range adjectives {
		for _, n := range nouns {
			if err := ValidateName(a + "-" + n); err != nil {
				t.Fatal(err)
			}
		}
	}
	name, err := RandomName()
	if err != nil || strings.Count(name, "-") != 1 || ValidateName(name) != nil {
		t.Errorf("RandomName = %q, %v", name, err)
	}
}

func TestFreeName(t *testing.T) {
	w := newWorld(t)
	name, err := FreeName(context.Background(), w.env(), ns)
	if err != nil || ValidateName(name) != nil {
		t.Errorf("FreeName = %q, %v", name, err)
	}

	// Every name taken: give up.
	taken := newWorld(t)
	taken.dyn.PrependReactor("get", "virtualmachines", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, vm(a.(k8stesting.GetAction).GetName(), "Running"), nil
	})
	if _, err := FreeName(context.Background(), taken.env(), ns); err == nil || !strings.Contains(err.Error(), "no free random name") {
		t.Errorf("all taken: %v", err)
	}
	broken := newWorld(t)
	broken.dyn.PrependReactor("get", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errBoom })
	if _, err := FreeName(context.Background(), broken.env(), ns); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("read error: %v", err)
	}
}

func TestNames(t *testing.T) {
	w := newWorld(t, vm("ready", "Running"), vm("off", "Stopped"))
	// A vSNO still building its ISO has only the Service.
	fresh := svc("building", "172.30.1.9")
	fresh.Labels = map[string]string{LabelName: "building"}
	// A Service not labeled by vsno is not a vSNO.
	other := svc("dnsmasq-other", "172.30.0.54")
	for _, s := range []*corev1.Service{fresh, other} {
		if _, err := w.kube.CoreV1().Services(ns).Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Names(context.Background(), w.kube, w.dyn, ns)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"building", "off", "ready"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
}

func TestNamesEmpty(t *testing.T) {
	w := newWorld(t)
	got, err := Names(context.Background(), w.kube, w.dyn, ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("Names on an empty environment = %v, want none", got)
	}
}

func TestCreateWithoutName(t *testing.T) {
	w := newWorld(t)
	g, err := Create(context.Background(), w.env(), createOpts("", "4.20"))
	if err != nil || ValidateName(g.Name) != nil || !strings.Contains(g.Name, "-") {
		t.Errorf("%v %+v", err, g)
	}
}
