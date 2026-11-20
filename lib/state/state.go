// Package state stores a vSNO environment's settings and progress in the
// ConfigMap <namespace>/vsno-state, so later commands can find and resume it.
package state

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ConfigMapName holds the state in the environment's namespace.
	ConfigMapName = "vsno-state"
	key           = "state.json"
)

// StageStatus is the outcome of a recorded stage.
type StageStatus string

const (
	Running StageStatus = "running"
	Done    StageStatus = "done"
	Failed  StageStatus = "failed"
)

// Stage is one recorded step, e.g. "setup" succeeded at a time.
type Stage struct {
	Name   string      `json:"name"`
	Status StageStatus `json:"status"`
	Time   time.Time   `json:"time"`
	Detail string      `json:"detail,omitempty"`
}

// State is everything vsno remembers about an environment.
type State struct {
	Node    string `json:"node,omitempty"`
	Disk    string `json:"disk,omitempty"`
	HostDNS string `json:"hostDNS,omitempty"`
	// Services maps a support Service name to its ClusterIP.
	Services map[string]string `json:"services,omitempty"`
	// Versions maps a component (e.g. operator CSV) to its version.
	Versions map[string]string `json:"versions,omitempty"`
	Stages   []Stage           `json:"stages,omitempty"`
	// PreviousDensity is HyperConverged's density settings before setup
	// first changed them, which teardown puts back; nil when setup didn't.
	PreviousDensity *Density `json:"previousDensity,omitempty"`
}

// Density is how tightly OpenShift Virtualization packs VMs: memory
// overcommit (each VM's pod requests guest memory ÷ percent/100) and kernel
// samepage merging on one node.
type Density struct {
	// OvercommitPercent is memoryOvercommitPercentage; 0 means unset (100).
	OvercommitPercent int `json:"overcommitPercent,omitempty"`
	// KSMNode is the node KSM is limited to; "" means KSM is off.
	KSMNode string `json:"ksmNode,omitempty"`
}

// Percent is the overcommit percentage in effect: 100 when unset.
func (d Density) Percent() int {
	if d.OvercommitPercent <= 0 {
		return 100
	}
	return d.OvercommitPercent
}

// maxStages bounds the history kept in the ConfigMap.
const maxStages = 50

// Record appends a stage, keeping the most recent maxStages.
func (s *State) Record(name string, status StageStatus, detail string, now time.Time) {
	s.Stages = append(s.Stages, Stage{Name: name, Status: status, Time: now.UTC(), Detail: detail})
	if n := len(s.Stages); n > maxStages {
		s.Stages = s.Stages[n-maxStages:]
	}
}

// Last returns the most recent stage with this name, if any.
func (s *State) Last(name string) (Stage, bool) {
	for i := len(s.Stages) - 1; i >= 0; i-- {
		if s.Stages[i].Name == name {
			return s.Stages[i], true
		}
	}
	return Stage{}, false
}

// Load reads the state. A missing ConfigMap (or namespace) returns an empty
// State and found=false.
func Load(ctx context.Context, kube kubernetes.Interface, ns string) (st *State, found bool, err error) {
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &State{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %s/%s: %w", ns, ConfigMapName, err)
	}
	st = &State{}
	if err := json.Unmarshal([]byte(cm.Data[key]), st); err != nil {
		return nil, true, fmt.Errorf("parsing %s/%s: %w", ns, ConfigMapName, err)
	}
	return st, true, nil
}

// Save writes the state, creating the ConfigMap if needed.
func Save(ctx context.Context, kube kubernetes.Interface, ns string, st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	cms := kube.CoreV1().ConfigMaps(ns)
	cm, err := cms.Get(ctx, ConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "vsno"}},
			Data:       map[string]string{key: string(b)},
		}, metav1.CreateOptions{})
		return wrap(err, ns)
	}
	if err != nil {
		return wrap(err, ns)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = string(b)
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return wrap(err, ns)
}

func wrap(err error, ns string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("writing %s/%s: %w", ns, ConfigMapName, err)
}
