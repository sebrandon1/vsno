package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/manifests"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
)

// sizingRunner is just enough runner to exercise registrySize.
func sizingRunner(t *testing.T, want string, expand *bool) (*runner, *kfake.Clientset) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: registryPVC},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")},
		}},
	}
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: manifests.DefaultStorageClass}, AllowVolumeExpansion: expand}
	k := kfake.NewClientset(pvc, sc)
	r := &runner{
		Env: Env{Kube: k},
		o:   Options{Namespace: ns},
		res: &Result{},
		cfg: manifests.Config{Namespace: ns, StorageClass: manifests.DefaultStorageClass, RegistrySize: want},
	}
	return r, k
}

func yes() *bool { v := true; return &v }

func TestRegistrySize(t *testing.T) {
	ctx := context.Background()
	t.Run("grows an expandable volume", func(t *testing.T) {
		r, _ := sizingRunner(t, "200Gi", yes())
		if err := r.registrySize(ctx); err != nil {
			t.Fatal(err)
		}
		if r.cfg.RegistrySize != "200Gi" || len(r.res.Warnings) != 0 {
			t.Errorf("size = %s, warnings %q", r.cfg.RegistrySize, r.res.Warnings)
		}
	})
	t.Run("keeps an existing larger volume", func(t *testing.T) {
		r, _ := sizingRunner(t, "50Gi", yes())
		if err := r.registrySize(ctx); err != nil {
			t.Fatal(err)
		}
		if r.cfg.RegistrySize != "100Gi" {
			t.Errorf("size = %s, want the existing 100Gi", r.cfg.RegistrySize)
		}
		if len(r.res.Warnings) != 1 || !strings.Contains(r.res.Warnings[0], "can only grow") {
			t.Errorf("warnings = %q", r.res.Warnings)
		}
	})
	t.Run("refuses to grow a volume the StorageClass can't expand", func(t *testing.T) {
		r, _ := sizingRunner(t, "200Gi", nil)
		err := r.registrySize(ctx)
		if err == nil || !strings.Contains(err.Error(), "doesn't allow volume expansion") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("no existing volume", func(t *testing.T) {
		r, k := sizingRunner(t, "200Gi", nil)
		_ = k.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, registryPVC, metav1.DeleteOptions{})
		if err := r.registrySize(ctx); err != nil || r.cfg.RegistrySize != "200Gi" {
			t.Errorf("size = %s, %v", r.cfg.RegistrySize, err)
		}
	})
	t.Run("bad size", func(t *testing.T) {
		r, _ := sizingRunner(t, "lots", yes())
		if err := r.registrySize(ctx); err == nil || !strings.Contains(err.Error(), "registry size") {
			t.Errorf("got %v", err)
		}
	})
}
