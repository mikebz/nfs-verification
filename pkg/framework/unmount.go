package framework

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Force-deleting a mounted pod is the first half of F-001. The pod leaves the
// API before kubelet has unmounted, the wait that follows returns immediately
// because the object is gone, the claim is deleted, the provisioner destroys
// the export, and a node is left retrying RPCs against an export that no longer
// exists, uninterruptibly, on a hard NFSv4.1 mount.
//
// A case that needs a client to vanish without unlocking therefore does the
// same sequence with an observation between the two steps: force delete, watch
// the node until the mount is gone, and only then let teardown near the claim.
//
// The wait is never best effort. Expiry fails the case, naming the node and the
// volume, and teardown keeps the claim rather than deleting it under a live
// mount. A leaked claim is recoverable; a wedged node is not.

// UnmountTimeout bounds the wait for a node to release a volume after the pod
// using it was force-deleted. Generous, because kubelet's unmount runs after
// the object has already gone and there is no Pod left for its errors to land
// on as Events; short of forever, because a node that has not unmounted is a
// state to report, not to wait out.
const UnmountTimeout = 5 * time.Minute

// CSIUniqueVolumeName builds the name kubelet records in
// node.status.volumesInUse for a CSI volume.
//
// The entries there are kubelet UniqueVolumeNames, not raw CSI volume handles.
// For a CSI volume the plugin joins driver and handle with "^"
// (volNameSep in pkg/volume/csi/csi_plugin.go) and kubelet prefixes the plugin
// name, giving kubernetes.io/csi/<driver>^<handle>. Comparing a bare
// VolumeHandle against that list matches nothing, so the volume would read as
// gone on the first poll, which is F-001 with extra steps.
func CSIUniqueVolumeName(driver, handle string) (string, error) {
	if driver == "" || handle == "" {
		return "", fmt.Errorf("cannot build a unique volume name from driver %q and handle %q", driver, handle)
	}
	return "kubernetes.io/csi/" + driver + "^" + handle, nil
}

// attachRequired reports whether a CSI driver attaches its volumes, which
// decides whether node.status.volumesInUse says anything about them.
//
// The field's own definition is narrower than it reads: "List of attachable
// volumes in use (mounted) by the node". NFS CSI drivers generally set
// attachRequired false, because there is nothing to attach and the mount is the
// whole operation. On such a driver an empty volumesInUse is indistinguishable
// from a finished unmount. A driver with no CSIDriver object, or an in-tree
// spec.nfs volume, is treated the same way.
func (f *Framework) attachRequired(ctx context.Context, driver string) bool {
	if driver == "" {
		return false
	}
	d, err := f.C.Kube.StorageV1().CSIDrivers().Get(ctx, driver, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return d.Spec.AttachRequired != nil && *d.Spec.AttachRequired
}

// ForceDeletePodAndAwaitUnmount removes a pod from the API without a grace
// period and does not return until the node it ran on has released the volume
// behind the named claim.
//
// The claim is marked unproven before the pod is deleted and unmarked only on
// an observed unmount, so a case that fails between the two steps still cannot
// reach teardown with a live mount behind it. Registering the wait is not
// enough on its own: Defer callbacks return nothing, and the force-deleted pod
// is not in the API for teardown to notice.
func (f *Framework) ForceDeletePodAndAwaitUnmount(ctx context.Context, pod, claim string) error {
	name := f.Name(pod)
	p, err := f.C.Kube.CoreV1().Pods(Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading pod %s before force-deleting it: %w", name, err)
	}
	node := p.Spec.NodeName
	if node == "" {
		return fmt.Errorf("pod %s is not scheduled, so there is no node to watch for its unmount", name)
	}
	pv, err := f.PVForClaim(ctx, claim)
	if err != nil {
		return fmt.Errorf("finding the volume behind claim %s: %w", claim, err)
	}

	f.MarkClaimUnproven(f.Name(claim), fmt.Sprintf("pod %s was force-deleted while mounting it on %s", name, node))
	if err := f.DeletePodNow(ctx, pod); err != nil {
		return fmt.Errorf("force-deleting pod %s: %w", name, err)
	}
	if err := f.AwaitUnmount(ctx, node, pv, UnmountTimeout); err != nil {
		return err
	}
	f.ClearClaimUnproven(f.Name(claim))
	return nil
}

// AwaitUnmount waits until a node has released a volume, by whichever of the
// two observations applies.
//
// Both are always available. Preflight creates the privileged node agent and
// fails if it cannot schedule, and nothing runs until preflight passes, so the
// /proc/mounts path is never missing. The API path is an optimisation for
// drivers that attach, not a fallback: where it applies the wait is a Node GET
// instead of an exec into a privileged pod.
func (f *Framework) AwaitUnmount(ctx context.Context, node string, pv *corev1.PersistentVolume, timeout time.Duration) error {
	if unique, ok := f.uniqueVolumeName(ctx, pv); ok {
		return f.awaitVolumeNotInUse(ctx, node, unique, timeout)
	}
	return f.awaitMountGone(ctx, node, pv.Name, timeout)
}

// uniqueVolumeName returns the kubelet name for a volume when watching
// volumesInUse is meaningful for it, and false otherwise.
//
// A name it cannot construct is never treated as gone. Falling back to the node
// is the safe answer; reporting the volume released because a string could not
// be built is the unsafe one.
func (f *Framework) uniqueVolumeName(ctx context.Context, pv *corev1.PersistentVolume) (string, bool) {
	if pv.Spec.CSI == nil {
		return "", false
	}
	if !f.attachRequired(ctx, pv.Spec.CSI.Driver) {
		return "", false
	}
	unique, err := CSIUniqueVolumeName(pv.Spec.CSI.Driver, pv.Spec.CSI.VolumeHandle)
	if err != nil {
		return "", false
	}
	return unique, true
}

// awaitVolumeNotInUse watches node.status.volumesInUse for an attaching driver.
func (f *Framework) awaitVolumeNotInUse(ctx context.Context, node, unique string, timeout time.Duration) error {
	err := Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		n, err := f.C.Kube.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			// Not proven gone. An unreadable Node is a reason to keep waiting,
			// never a reason to report the volume released.
			return false, err
		}
		for _, v := range n.Status.VolumesInUse {
			if string(v) == unique {
				return false, fmt.Errorf("node %s still reports %s in volumesInUse", node, unique)
			}
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("node %s did not release %s within %s: deleting the claim now would destroy "+
			"the export under a live mount, and a hard NFSv4.1 mount retries that forever (docs/findings.md "+
			"F-001). The claim is kept; clean it up by hand once the node recovers. %w",
			node, unique, timeout, err)
	}
	return nil
}

// awaitMountGone watches /proc/mounts on the node, for a driver that does not
// attach and for in-tree spec.nfs volumes.
func (f *Framework) awaitMountGone(ctx context.Context, node, pvName string, timeout time.Duration) error {
	agent, err := NodeAgent(ctx, f.C)
	if err != nil {
		return fmt.Errorf("the node agent is the only way to see the unmount of %s on %s, and it is "+
			"unavailable: %w", pvName, node, err)
	}
	err = Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		mounts, err := agent.Mounts(ctx, node)
		if err != nil {
			return false, err
		}
		var still []string
		for _, m := range mounts {
			if isNFS(m.FSType) && strings.Contains(m.MountPoint, pvName) {
				still = append(still, m.MountPoint)
			}
		}
		if len(still) == 0 {
			return true, nil
		}
		return false, fmt.Errorf("node %s still mounts %s at %s", node, pvName, strings.Join(still, ", "))
	})
	if err != nil {
		return fmt.Errorf("node %s did not unmount volume %s within %s: deleting the claim now would "+
			"destroy the export under a live mount, and a hard NFSv4.1 mount retries that forever "+
			"(docs/findings.md F-001). The claim is kept; clean it up by hand once the node recovers. %w",
			node, pvName, timeout, err)
	}
	return nil
}
