package framework

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
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
	if err := CheckObjectName("claim", f.Name(spec.Name)); err != nil {
		return nil, err
	}
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
	// client-go's own retry backs off between attempts; a tight loop can spend
	// every attempt inside the window where the resource version has not
	// changed yet, and report a conflict that a moment's wait would have
	// resolved.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
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
//
// No condition at all is the third answer, and the one that costs the most time
// to work out from the outside: the claim was accepted for resize and nothing
// ever picked it up. A StorageClass may advertise allowVolumeExpansion whether
// or not the provisioner behind it can perform one, so this says where to look
// rather than leaving a bare timeout.
func describeResizeConditions(pvc *corev1.PersistentVolumeClaim) string {
	var parts []string
	for _, c := range pvc.Status.Conditions {
		parts = append(parts, fmt.Sprintf("%s=%s(%s)", c.Type, c.Status, c.Message))
	}
	if len(parts) == 0 {
		return " [no resize condition was ever posted on the claim: nothing acted on the request. " +
			"The StorageClass advertises allowVolumeExpansion, so check whether its provisioner supports " +
			"expansion at all and whether an external-resizer sidecar is running alongside the CSI driver]"
	}
	return " [" + strings.Join(parts, " ") + "]"
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

// UnroutableServer is an address from the range RFC 5737 reserves for
// documentation. Nothing routes to it on any network, which is what makes it
// safe to point a deliberately broken mount at.
const UnroutableServer = "192.0.2.1"

// BrokenNFSSpec describes a volume whose export does not exist.
type BrokenNFSSpec struct {
	Name    string
	Server  string
	Path    string
	Size    string
	Options []string
}

// CreateBrokenNFSVolume creates a PV pointing at an export that is not there,
// and a claim bound to it by name. It is how the mount-failure case produces a
// failure on the client without touching the real export or the real server.
//
// The PV is cluster-scoped, so teardown by namespace label cannot reach it. The
// caller gets a cleanup registered on the fixture rather than being trusted to
// remember.
func (f *Framework) CreateBrokenNFSVolume(ctx context.Context, spec BrokenNFSSpec) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim, error) {
	if spec.Server == "" {
		spec.Server = UnroutableServer
	}
	if spec.Path == "" {
		spec.Path = "/export/does-not-exist"
	}
	if spec.Size == "" {
		spec.Size = "1Gi"
	}
	if len(spec.Options) == 0 {
		// Bounded retries so mount.nfs gives up and reports, rather than
		// retrying quietly past the case's budget. This is the one mount in the
		// suite that is allowed to be soft: it is meant to fail, and a hard
		// mount here would hang the kubelet volume manager instead of
		// producing the Event the case is looking for.
		spec.Options = []string{"vers=4.1", "soft", "timeo=30", "retrans=2", "retry=1"}
	}
	var pv corev1.PersistentVolume
	if err := render("static-nfs-pv.yaml", map[string]any{
		"Name": f.Name(spec.Name), "Labels": f.Labels(), "Size": spec.Size,
		"Server": spec.Server, "Path": spec.Path, "Options": spec.Options,
	}, &pv); err != nil {
		return nil, nil, err
	}
	created, err := f.C.Kube.CoreV1().PersistentVolumes().Create(ctx, &pv, metav1.CreateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("creating the broken PV: %w", err)
	}
	f.Defer(func(ctx context.Context) {
		_ = IgnoreNotFound(f.C.Kube.CoreV1().PersistentVolumes().Delete(ctx, created.Name, metav1.DeleteOptions{}))
	})

	qty, err := resource.ParseQuantity(spec.Size)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing size %q: %w", spec.Size, err)
	}
	empty := ""
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: f.Name(spec.Name), Namespace: Namespace, Labels: f.Labels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			// By name and with an empty class: this claim must bind to the
			// broken volume and to nothing else, least of all to a real one.
			VolumeName:       created.Name,
			StorageClassName: &empty,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	boundClaim, err := f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).Create(ctx, claim, metav1.CreateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("creating the claim for the broken PV: %w", err)
	}
	return created, boundClaim, nil
}

// SetPVReclaimPolicy changes the reclaim policy on a PersistentVolume.
func (f *Framework) SetPVReclaimPolicy(ctx context.Context, pvName string, policy corev1.PersistentVolumeReclaimPolicy) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pv, err := f.C.Kube.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		pv.Spec.PersistentVolumeReclaimPolicy = policy
		_, err = f.C.Kube.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		return err
	})
}

// ReleasePV clears the ClaimRef on a retained PersistentVolume so it transitions
// from Released to Available and can be rebound by a new claim.
func (f *Framework) ReleasePV(ctx context.Context, pvName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pv, err := f.C.Kube.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		pv.Spec.ClaimRef = nil
		_, err = f.C.Kube.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		return err
	})
}

// WaitPVPhase waits for a PersistentVolume to reach the desired phase (e.g. Released or Available).
func (f *Framework) WaitPVPhase(ctx context.Context, pvName string, phase corev1.PersistentVolumePhase, timeout time.Duration) error {
	return Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pv, err := f.C.Kube.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pv.Status.Phase == phase {
			return true, nil
		}
		return false, fmt.Errorf("pv %s in phase %s, want %s", pvName, pv.Status.Phase, phase)
	})
}

// BindPVToClaim creates a claim explicitly targeting a pre-existing PersistentVolume by name.
func (f *Framework) BindPVToClaim(ctx context.Context, name, pvName, size string) (*corev1.PersistentVolumeClaim, error) {
	if size == "" {
		size = Cfg().PVCSize
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("parsing size %q: %w", size, err)
	}
	if err := CheckObjectName("claim", f.Name(name)); err != nil {
		return nil, err
	}
	pv, err := f.C.Kube.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading target PV %s: %w", pvName, err)
	}
	sc := pv.Spec.StorageClassName
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: f.Name(name), Namespace: Namespace, Labels: f.Labels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			VolumeName:       pvName,
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	return f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).Create(ctx, claim, metav1.CreateOptions{})
}

var (
	// VolumeSnapshotGVR identifies the VolumeSnapshot custom resource.
	VolumeSnapshotGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshots",
	}
	// VolumeSnapshotClassGVR identifies the VolumeSnapshotClass custom resource.
	VolumeSnapshotClassGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshotclasses",
	}
)

// VolumeSnapshotClasses returns names of all available VolumeSnapshotClass resources.
func (c *Client) VolumeSnapshotClasses(ctx context.Context) ([]string, error) {
	if c.Dynamic == nil {
		return nil, fmt.Errorf("dynamic client not configured")
	}
	list, err := c.Dynamic.Resource(VolumeSnapshotClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	return names, nil
}

// MatchingVolumeSnapshotClass returns the name of a VolumeSnapshotClass configured for the given driver, if any.
func (c *Client) MatchingVolumeSnapshotClass(ctx context.Context, driver string) (string, error) {
	if c.Dynamic == nil {
		return "", fmt.Errorf("dynamic client not configured")
	}
	list, err := c.Dynamic.Resource(VolumeSnapshotClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, item := range list.Items {
		d, _, _ := unstructured.NestedString(item.Object, "driver")
		if d == driver {
			return item.GetName(), nil
		}
	}
	return "", nil
}

// CreateVolumeSnapshot creates a VolumeSnapshot resource targeting a PVC.
func (f *Framework) CreateVolumeSnapshot(ctx context.Context, snapName, pvcName, className string) (*unstructured.Unstructured, error) {
	if f.C.Dynamic == nil {
		return nil, fmt.Errorf("dynamic client not configured")
	}
	spec := map[string]any{
		"source": map[string]any{
			"persistentVolumeClaimName": f.Name(pvcName),
		},
	}
	if className != "" {
		spec["volumeSnapshotClassName"] = className
	}
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshot",
			"metadata": map[string]any{
				"name":      f.Name(snapName),
				"namespace": Namespace,
				"labels":    f.Labels(),
			},
			"spec": spec,
		},
	}
	created, err := f.C.Dynamic.Resource(VolumeSnapshotGVR).Namespace(Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	f.Defer(func(ctx context.Context) {
		_ = f.DeleteVolumeSnapshot(ctx, snapName)
	})
	return created, nil
}

// WaitVolumeSnapshotReady waits for a VolumeSnapshot to have status.readyToUse == true.
func (f *Framework) WaitVolumeSnapshotReady(ctx context.Context, snapName string, timeout time.Duration) error {
	if f.C.Dynamic == nil {
		return fmt.Errorf("dynamic client not configured")
	}
	name := f.Name(snapName)
	return Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		snap, err := f.C.Dynamic.Resource(VolumeSnapshotGVR).Namespace(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		status, ok := snap.Object["status"].(map[string]any)
		if !ok {
			return false, fmt.Errorf("snapshot %s has no status yet", name)
		}
		ready, _ := status["readyToUse"].(bool)
		if ready {
			return true, nil
		}
		return false, fmt.Errorf("snapshot %s readyToUse is false", name)
	})
}

// DeleteVolumeSnapshot removes a VolumeSnapshot by logical name.
func (f *Framework) DeleteVolumeSnapshot(ctx context.Context, snapName string) error {
	if f.C.Dynamic == nil {
		return fmt.Errorf("dynamic client not configured")
	}
	return IgnoreNotFound(f.C.Dynamic.Resource(VolumeSnapshotGVR).Namespace(Namespace).
		Delete(ctx, f.Name(snapName), metav1.DeleteOptions{}))
}

// ChurnResult records the outcome of a rapid create/delete churn run.
type ChurnResult struct {
	Completed int
	Errors    []error
}

// RunPVCLifecycleChurn executes cycles iterations of creating an RWX PVC, waiting for
// it to bind, deleting it, and waiting for it to be removed.
func (f *Framework) RunPVCLifecycleChurn(ctx context.Context, cycles int, namePrefix string) (ChurnResult, error) {
	var res ChurnResult
	for i := 1; i <= cycles; i++ {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		cName := fmt.Sprintf("%s-%d", namePrefix, i)
		pvc, err := f.CreatePVC(ctx, PVCSpec{Name: cName})
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("cycle %d create: %w", i, err))
			return res, fmt.Errorf("cycle %d create failed: %w", i, err)
		}
		if _, err := f.WaitPVCBound(ctx, pvc.Name, BindTimeout); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("cycle %d bind: %w", i, err))
			return res, fmt.Errorf("cycle %d bind failed: %w", i, err)
		}
		if err := f.DeletePVC(ctx, pvc.Name); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("cycle %d delete: %w", i, err))
			return res, fmt.Errorf("cycle %d delete failed: %w", i, err)
		}
		if err := f.WaitPVCGone(ctx, pvc.Name, DeleteTimeout); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("cycle %d wait gone: %w", i, err))
			return res, fmt.Errorf("cycle %d wait gone failed: %w", i, err)
		}
		res.Completed++
	}
	return res, nil
}
