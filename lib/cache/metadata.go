package cache

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// metadata is ConfigMap <ns>/vsno-cache's contents: what is mirrored, by
// payload digest. The images themselves live in the registry, so this only
// records what the registry can't tell vsno: the version, when it was mirrored
// and when it was last used.
type metadata struct {
	Entries map[string]*Entry `json:"entries"`
}

// load reads the metadata; a missing ConfigMap means nothing is cached.
func load(ctx context.Context, kube kubernetes.Interface, ns string) (*metadata, error) {
	m, _, err := loadCM(ctx, kube, ns)
	return m, err
}

// loadCM also returns the ConfigMap, so update can write it back without a
// second read (a conflict would otherwise be unrecoverable).
func loadCM(ctx context.Context, kube kubernetes.Interface, ns string) (*metadata, *corev1.ConfigMap, error) {
	cm, err := kube.CoreV1().ConfigMaps(ns).Get(ctx, ConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &metadata{Entries: map[string]*Entry{}}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the cache's records: %w", err)
	}
	m := &metadata{}
	if raw := cm.Data[key]; raw != "" {
		if err := json.Unmarshal([]byte(raw), m); err != nil {
			return nil, nil, fmt.Errorf("reading ConfigMap %s/%s: %w", ns, ConfigMap, err)
		}
	}
	if m.Entries == nil {
		m.Entries = map[string]*Entry{}
	}
	return m, cm, nil
}

// update changes the metadata and saves it, retrying if someone else wrote it
// in the meantime.
func update(ctx context.Context, kube kubernetes.Interface, ns string, change func(*metadata)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		m, cm, err := loadCM(ctx, kube, ns)
		if err != nil {
			return err
		}
		change(m)
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if cm == nil {
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ConfigMap,
					Labels: map[string]string{"app.kubernetes.io/managed-by": "vsno"}},
				Data: map[string]string{key: string(b)},
			}
			_, err = kube.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
		} else {
			cm.Data = map[string]string{key: string(b)}
			_, err = kube.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})
		}
		if err != nil {
			return fmt.Errorf("saving the cache's records: %w", err)
		}
		return nil
	})
}
