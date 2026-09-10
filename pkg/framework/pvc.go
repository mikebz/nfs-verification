package framework

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PVCSpec describes a claim the harness creates.
type PVCSpec struct {
	Name         string
	Size         string
	StorageClass string
	AccessMode   corev1.PersistentVolumeAccessMode
	DataSource   *corev1.TypedLocalObjectReference
}

// CreatePVC creates a claim in the framework namespace. Defaults are RWX at the
// configured size on the discovered RWX StorageClass.
func (f *Framework) CreatePVC(ctx context.Context, spec PVCSpec) (*corev1.PersistentVolumeClaim, error) {
	if spec.Size == "" {
		spec.Size = Cfg().PVCSize
	}
	if spec.StorageClass == "" {
		spec.StorageClass = f.Env.StorageClass
	}
	if spec.AccessMode == "" {
		spec.AccessMode = corev1.ReadWriteMany
	}
	qty, err := resource.ParseQuantity(spec.Size)
	if err != nil {
		return nil, fmt.Errorf("parsing size %q: %w", spec.Size, err)
	}
	sc := spec.StorageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: f.Namespace, Labels: f.Labels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{spec.AccessMode},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
			DataSource: spec.DataSource,
		},
	}
	return f.C.Kube.CoreV1().PersistentVolumeClaims(f.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
}

// WaitPVCBound waits for a claim to reach Bound.
func (f *Framework) WaitPVCBound(ctx context.Context, name string, timeout time.Duration) (*corev1.PersistentVolumeClaim, error) {
	var bound *corev1.PersistentVolumeClaim
	err := Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pvc, err := f.C.Kube.CoreV1().PersistentVolumeClaims(f.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pvc.Status.Phase == corev1.ClaimBound {
			bound = pvc
			return true, nil
		}
		return false, fmt.Errorf("pvc %s is %s", name, pvc.Status.Phase)
	})
	return bound, err
}

// MustBoundRWXPVC creates an RWX claim and waits for it to bind.
func (f *Framework) MustBoundRWXPVC(ctx context.Context, name string) *corev1.PersistentVolumeClaim {
	f.T.Helper()
	if _, err := f.CreatePVC(ctx, PVCSpec{Name: name}); err != nil {
		f.T.Fatalf("creating RWX PVC %s: %v", name, err)
	}
	pvc, err := f.WaitPVCBound(ctx, name, BindTimeout)
	if err != nil {
		f.T.Fatalf("RWX PVC %s did not bind: %v", name, err)
	}
	return pvc
}

// RWXStorageClasses returns every StorageClass in the cluster, so preflight can
// probe them rather than trusting an annotation about access modes: nothing in
// the StorageClass API states which access modes its PVs will support.
func (c *Client) StorageClasses(ctx context.Context) ([]storagev1.StorageClass, error) {
	list, err := c.Kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// SupportsExpansion reports whether the StorageClass advertises expansion.
func (c *Client) SupportsExpansion(ctx context.Context, name string) (bool, error) {
	sc, err := c.Kube.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion, nil
}

// PVForClaim returns the PersistentVolume bound to a claim.
func (f *Framework) PVForClaim(ctx context.Context, name string) (*corev1.PersistentVolume, error) {
	pvc, err := f.C.Kube.CoreV1().PersistentVolumeClaims(f.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if pvc.Spec.VolumeName == "" {
		return nil, fmt.Errorf("claim %s is not bound", name)
	}
	return f.C.Kube.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
}

// WaitPVGone waits for a PersistentVolume to be removed, which is how the
// lifecycle cases assert that backing storage was actually reclaimed rather
// than merely unbound.
func (f *Framework) WaitPVGone(ctx context.Context, name string, timeout time.Duration) error {
	return Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pv, err := f.C.Kube.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return IgnoreNotFound(err) == nil, nil
		}
		return false, fmt.Errorf("pv %s still present in phase %s", name, pv.Status.Phase)
	})
}

// WaitPVCGone waits for a claim to disappear.
func (f *Framework) WaitPVCGone(ctx context.Context, name string, timeout time.Duration) error {
	return Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		_, err := f.C.Kube.CoreV1().PersistentVolumeClaims(f.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return IgnoreNotFound(err) == nil, nil
		}
		return false, fmt.Errorf("pvc %s still present", name)
	})
}
