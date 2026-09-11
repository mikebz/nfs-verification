package framework

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mount options belong to the volume, not to the pod: Kubernetes has no
// per-pod override, and a second StorageClass would give a different export.
// So a case that needs one file seen through two different mounts needs two
// volumes over one export, which means reading the server and path out of the
// volume a dynamic claim already owns.
//
// That extraction is the fragile part, and it is fragile in a way a unit test
// can hold, so it is a pure function over a PV object.

// NFSSource is the export behind a PersistentVolume.
type NFSSource struct {
	Server string
	Path   string
}

func (s NFSSource) String() string { return s.Server + ":" + s.Path }

// nfsServerKeys are the volumeAttributes keys drivers use for the server. The
// harness knows the common ones and nothing more: a guess that lands on the
// wrong attribute produces a clone pointing at an export nobody asked for.
var nfsServerKeys = []string{"server", "nfsServer", "nfs-server", "nfsserver"}

// nfsPathKeys are the keys drivers use for the export path.
var nfsPathKeys = []string{"share", "path", "nfsShare", "nfs-share", "nfsshare"}

// nfsSubDirKeys are the keys for a subdirectory the driver appends to the
// export. A clone that ignored one would mount the parent export and see a
// different directory from the claim it is meant to shadow.
var nfsSubDirKeys = []string{"subdir", "subDir", "subdirectory"}

// ExtractNFSSource reads the server and export path out of a PersistentVolume.
//
// Two shapes, and a third answer. spec.nfs states both directly, which is what
// an in-cluster provisioner produces. spec.csi puts them in volumeAttributes
// under keys the driver chooses. Anything else is an error naming the volume
// and the attributes it did carry, so an operator can see which key to add
// rather than being told the case is unsupported.
func ExtractNFSSource(pv *corev1.PersistentVolume) (NFSSource, error) {
	if pv == nil {
		return NFSSource{}, fmt.Errorf("no volume to read an export from")
	}
	if n := pv.Spec.NFS; n != nil {
		if n.Server == "" || n.Path == "" {
			return NFSSource{}, fmt.Errorf("volume %s has spec.nfs with server %q and path %q, and a "+
				"clone needs both", pv.Name, n.Server, n.Path)
		}
		return NFSSource{Server: n.Server, Path: n.Path}, nil
	}
	c := pv.Spec.CSI
	if c == nil {
		return NFSSource{}, fmt.Errorf("volume %s is neither an in-tree spec.nfs volume nor a CSI volume, "+
			"so there is no export to mount a second time", pv.Name)
	}
	server := firstAttribute(c.VolumeAttributes, nfsServerKeys)
	path := firstAttribute(c.VolumeAttributes, nfsPathKeys)
	if server == "" || path == "" {
		return NFSSource{}, fmt.Errorf("volume %s is a CSI volume on driver %s and its volumeAttributes "+
			"do not name an export: looked for a server under %s and a path under %s, and the volume "+
			"carries %s. The harness does not guess an attribute, because a clone pointing at the wrong "+
			"export is worse than a case that did not run",
			pv.Name, c.Driver, strings.Join(nfsServerKeys, ", "), strings.Join(nfsPathKeys, ", "),
			describeAttributes(c.VolumeAttributes))
	}
	if sub := firstAttribute(c.VolumeAttributes, nfsSubDirKeys); sub != "" {
		path = strings.TrimSuffix(path, "/") + "/" + strings.TrimPrefix(sub, "/")
	}
	return NFSSource{Server: server, Path: path}, nil
}

// firstAttribute returns the value of the first key present and non-empty.
func firstAttribute(attrs map[string]string, keys []string) string {
	for _, k := range keys {
		if v := attrs[k]; v != "" {
			return v
		}
	}
	return ""
}

// describeAttributes renders the attribute keys a volume carries, sorted, so a
// failure says what was there rather than only what was missing.
func describeAttributes(attrs map[string]string) string {
	if len(attrs) == 0 {
		return "no volumeAttributes at all"
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "volumeAttributes " + strings.Join(keys, ", ")
}

// CloneVolumeSpec describes a second mount of an export a dynamic claim already
// owns, with different mount options.
type CloneVolumeSpec struct {
	Name    string
	Source  NFSSource
	Size    string
	Options []string
}

// CloneVolume creates a static PersistentVolume over an existing export and a
// claim bound to it by name, so one file can be seen through two mounts with
// different options.
//
// The PV is cluster-scoped, so teardown by namespace label cannot reach it; the
// caller gets a cleanup registered on the fixture rather than being trusted to
// remember. Ordering is safe by the API rather than by luck: pv-protection
// holds the volume until its claim is gone, which happens after the pods are.
//
// Its reclaim policy is Retain, because nothing provisioned it and there is
// nothing to reclaim. The export belongs to the dynamic claim, which deletes it
// in the ordinary way.
func (f *Framework) CloneVolume(ctx context.Context, spec CloneVolumeSpec) (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim, error) {
	if spec.Source.Server == "" || spec.Source.Path == "" {
		return nil, nil, fmt.Errorf("a clone volume needs a server and a path, got %q", spec.Source)
	}
	if spec.Size == "" {
		spec.Size = Cfg().PVCSize
	}
	if len(spec.Options) == 0 {
		return nil, nil, fmt.Errorf("a clone volume with no mount options is the same mount twice, " +
			"which is the thing the cases using this exist to contrast against")
	}
	var pv corev1.PersistentVolume
	if err := render("static-nfs-pv.yaml", map[string]any{
		"Name": f.Name(spec.Name), "Labels": f.Labels(), "Size": spec.Size,
		"Server": spec.Source.Server, "Path": spec.Source.Path, "Options": spec.Options,
	}, &pv); err != nil {
		return nil, nil, err
	}
	created, err := f.C.Kube.CoreV1().PersistentVolumes().Create(ctx, &pv, metav1.CreateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("creating the clone PV over %s: %w", spec.Source, err)
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
			// By name and with an empty class: this claim binds to the clone and
			// to nothing else, least of all to a freshly provisioned volume.
			VolumeName:       created.Name,
			StorageClassName: &empty,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	bound, err := f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).Create(ctx, claim, metav1.CreateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("creating the claim for the clone PV: %w", err)
	}
	return created, bound, nil
}
