package framework

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Reading the export out of a bound volume is the fragile part of mounting one
// export twice, and it is fragile in a way a unit test can hold. A clone
// pointing at the wrong export would not fail: it would pass, having compared
// two pods looking at two different files.

// TestExtractNFSSource covers the shapes a bound RWX volume comes in.
//
// Steps:
//  1. Read an in-tree spec.nfs volume, which is what an in-cluster provisioner
//     produces.
//  2. Read a CSI volume under each of the attribute spellings the harness
//     knows.
//  3. Check a subdirectory attribute is appended, since ignoring one mounts the
//     parent export and shadows a different directory.
//  4. Assert a volume it cannot read is an error that names the volume and the
//     attributes it did carry, rather than a guess.
func TestExtractNFSSource(t *testing.T) {
	nfsPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-intree"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			NFS: &corev1.NFSVolumeSource{Server: "10.0.0.5", Path: "/export/pvc-intree"}}},
	}
	csiPV := func(attrs map[string]string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-csi"},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "nfs.csi.k8s.io", VolumeHandle: "h", VolumeAttributes: attrs}}},
		}
	}
	cases := []struct {
		name string
		pv   *corev1.PersistentVolume
		want NFSSource
	}{
		{name: "in-tree", pv: nfsPV, want: NFSSource{Server: "10.0.0.5", Path: "/export/pvc-intree"}},
		{
			name: "csi-server-and-share",
			pv:   csiPV(map[string]string{"server": "10.0.0.5", "share": "/export"}),
			want: NFSSource{Server: "10.0.0.5", Path: "/export"},
		},
		{
			name: "csi-alternate-spellings",
			pv:   csiPV(map[string]string{"nfsServer": "nfs.svc.cluster.local", "nfs-share": "/data"}),
			want: NFSSource{Server: "nfs.svc.cluster.local", Path: "/data"},
		},
		{
			name: "csi-with-subdir",
			pv:   csiPV(map[string]string{"server": "10.0.0.5", "share": "/export/", "subdir": "/pvc-123"}),
			want: NFSSource{Server: "10.0.0.5", Path: "/export/pvc-123"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractNFSSource(tc.pv)
			if err != nil {
				t.Fatalf("reading the export: %v", err)
			}
			if got != tc.want {
				t.Errorf("read %s, want %s", got, tc.want)
			}
		})
	}
}

// TestExtractNFSSourceRefusesToGuess covers the third answer. A volume whose
// export cannot be read must report blocked and say which fields were missing;
// a guess that lands on the wrong attribute produces a clone over an export
// nobody asked for, and the case then passes having compared two files.
func TestExtractNFSSourceRefusesToGuess(t *testing.T) {
	unreadable := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-opaque"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "vendor.csi.example.com", VolumeHandle: "h",
				VolumeAttributes: map[string]string{"endpoint": "10.0.0.5", "volumeId": "v-1"}}}},
	}
	_, err := ExtractNFSSource(unreadable)
	if err == nil {
		t.Fatal("an export was produced from attributes the harness does not know")
	}
	for _, want := range []string{"pvc-opaque", "vendor.csi.example.com", "endpoint", "volumeId", "server"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q, so an operator cannot see which key to add: %v", want, err)
		}
	}

	for _, pv := range []*corev1.PersistentVolume{
		nil,
		{ObjectMeta: metav1.ObjectMeta{Name: "pvc-empty"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pvc-half"}, Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: "10.0.0.5"}}}},
	} {
		if src, err := ExtractNFSSource(pv); err == nil {
			t.Errorf("produced %s from a volume with no readable export", src)
		}
	}
}
