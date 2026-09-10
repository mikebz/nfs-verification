package framework

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		ObjectMeta: metav1.ObjectMeta{Name: f.Name(spec.Name), Namespace: Namespace, Labels: f.Labels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{spec.AccessMode},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
			DataSource: spec.DataSource,
		},
	}
	return f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).Create(ctx, pvc, metav1.CreateOptions{})
}

// WaitPVCBound waits for a claim to reach Bound.
func (f *Framework) WaitPVCBound(ctx context.Context, name string, timeout time.Duration) (*corev1.PersistentVolumeClaim, error) {
	name = f.Name(name)
	var bound *corev1.PersistentVolumeClaim
	err := Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pvc, err := f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, name, metav1.GetOptions{})
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

// MustRWXPVC creates an RWX claim, and waits for it to bind when the class
// binds immediately. Under WaitForFirstConsumer it returns the claim unbound:
// such a class binds only once a pod referencing it is scheduled, so waiting
// here would deadlock against the case that is about to create that pod.
func (f *Framework) MustRWXPVC(ctx context.Context, name string) *corev1.PersistentVolumeClaim {
	f.T.Helper()
	pvc, err := f.CreatePVC(ctx, PVCSpec{Name: name})
	if err != nil {
		f.T.Fatalf("creating RWX PVC %s: %v", name, err)
	}
	mode, err := f.BindingMode(ctx, f.Env.StorageClass)
	if err != nil {
		f.T.Fatalf("reading the binding mode of StorageClass %s: %v", f.Env.StorageClass, err)
	}
	if mode == storagev1.VolumeBindingWaitForFirstConsumer {
		f.T.Logf("StorageClass %s binds on first consumer; %s stays Pending until a pod is scheduled",
			f.Env.StorageClass, pvc.Name)
		return pvc
	}
	bound, err := f.WaitPVCBound(ctx, name, BindTimeout)
	if err != nil {
		f.T.Fatalf("RWX PVC %s did not bind: %v", name, err)
	}
	return bound
}

// BindingMode reports when a StorageClass binds its volumes. An unset mode is
// Immediate, per the API's own default.
func (f *Framework) BindingMode(ctx context.Context, class string) (storagev1.VolumeBindingMode, error) {
	sc, err := f.C.Kube.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if sc.VolumeBindingMode == nil {
		return storagev1.VolumeBindingImmediate, nil
	}
	return *sc.VolumeBindingMode, nil
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
	pvc, err := f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, f.Name(name), metav1.GetOptions{})
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
	name = f.Name(name)
	return Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		_, err := f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return IgnoreNotFound(err) == nil, nil
		}
		return false, fmt.Errorf("pvc %s still present", name)
	})
}

// ExpandPVC requests a new size on an existing claim. It returns the API's
// error unwrapped, because a class that does not advertise expansion is
// expected to reject the request and the case asserts on that rejection.
func (f *Framework) ExpandPVC(ctx context.Context, name, size string) error {
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("parsing size %q: %w", size, err)
	}
	claims := f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace)
	// Read-modify-write, retried on conflict: the resize controller writes to
	// the same object, so a stale read here is ordinary rather than a defect.
	return retryOnConflict(func() error {
		pvc, err := claims.Get(ctx, f.Name(name), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1.ResourceList{}
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = qty
		_, err = claims.Update(ctx, pvc, metav1.UpdateOptions{})
		return err
	})
}

// WaitPVCCapacity waits for status.capacity to reach at least size. Status, not
// spec: spec is what was asked for, status is what the driver delivered.
func (f *Framework) WaitPVCCapacity(ctx context.Context, name, size string, timeout time.Duration) (resource.Quantity, error) {
	want, err := resource.ParseQuantity(size)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("parsing size %q: %w", size, err)
	}
	var got resource.Quantity
	err = Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pvc, err := f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, f.Name(name), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		got = pvc.Status.Capacity[corev1.ResourceStorage]
		if got.Cmp(want) >= 0 {
			return true, nil
		}
		return false, fmt.Errorf("claim %s reports %s, want at least %s%s",
			name, got.String(), want.String(), describeResizeConditions(pvc))
	})
	return got, err
}

// describeResizeConditions names the resize condition the driver left behind,
// which is the difference between "expansion is still running" and "expansion
// needs a pod restart the case is not doing".
func describeResizeConditions(pvc *corev1.PersistentVolumeClaim) string {
	var parts []string
	for _, c := range pvc.Status.Conditions {
		parts = append(parts, fmt.Sprintf("%s=%s(%s)", c.Type, c.Status, c.Message))
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, " ") + "]"
}

// retryOnConflict retries a read-modify-write a few times on a conflict.
func retryOnConflict(fn func() error) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = fn(); !apierrors.IsConflict(err) {
			return err
		}
	}
	return err
}

// GetPVC returns a claim by its logical name.
func (f *Framework) GetPVC(ctx context.Context, name string) (*corev1.PersistentVolumeClaim, error) {
	return f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).Get(ctx, f.Name(name), metav1.GetOptions{})
}

// DeletePVC deletes a claim. Deleting one a pod still mounts is a supported
// operation, not a hazard: the pvc-protection finalizer holds the object in
// Terminating until the mount is gone, which is exactly what PROV-03 asserts.
// The hazard in docs/findings.md F-001 is deleting the claim after the pod has
// been *force* removed from the API, which defeats that protection.
func (f *Framework) DeletePVC(ctx context.Context, name string) error {
	return IgnoreNotFound(f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).
		Delete(ctx, f.Name(name), metav1.DeleteOptions{}))
}
