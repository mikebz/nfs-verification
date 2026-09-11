package framework

import (
	"strings"
	"testing"
	"time"
)

// The kubelet is not on this workstation and its summary cannot be produced
// here, so these tests are written against recorded documents rather than
// against the real tool. What they are protecting is the reading of a document
// whose fields are optional: the kubelet omits what it does not have, and every
// wrong answer below reads as a healthy volume rather than as an error.

// summaryWithUsage is one node's summary as the kubelet serves it: a pod with a
// claim it reports usage for, and a second pod on the same node whose entry
// must not be mistaken for the first's.
const summaryWithUsage = `{
  "node": {"nodeName": "worker-1"},
  "pods": [
    {
      "podRef": {"name": "other-pod", "namespace": "default", "uid": "u1"},
      "volume": [
        {"time": "2026-09-11T10:00:00Z", "availableBytes": 1, "capacityBytes": 2, "usedBytes": 3,
         "name": "vol0", "pvcRef": {"name": "nfsv-obs-06-run-claim", "namespace": "default"}}
      ]
    },
    {
      "podRef": {"name": "nfsv-obs-06-run-writer", "namespace": "default", "uid": "u2"},
      "volume": [
        {"time": "2026-09-11T10:01:00Z", "availableBytes": 900, "capacityBytes": 1000, "usedBytes": 80,
         "name": "kube-api-access-abcde"},
        {"time": "2026-09-11T10:02:03Z", "availableBytes": 1038336, "capacityBytes": 1048576,
         "usedBytes": 10240, "name": "vol0",
         "pvcRef": {"name": "nfsv-obs-06-run-claim", "namespace": "default"}}
      ]
    }
  ]
}`

// TestKubeletSummaryVolumeLookup reads one claim's usage out of a node summary.
//
// Identity is the assertion, not the arithmetic. An RWX claim is mounted by
// several pods at once and each mount has its own entry, so a lookup that
// matched on the claim alone would answer with another pod's reading and the
// case would compare two different mounts.
//
// Steps:
//  1. Decode a recorded summary and look up the claim under the pod that holds it.
//  2. Check every field, including the kubelet's own sample time.
//  3. Look the same claim up under a pod that does not exist, and under the
//     right pod with the wrong claim.
func TestKubeletSummaryVolumeLookup(t *testing.T) {
	s := decodeSummary(t, summaryWithUsage)

	got, ok := s.VolumeUsageFor("default", "nfsv-obs-06-run-writer", "nfsv-obs-06-run-claim")
	if !ok {
		t.Fatalf("no reading for a claim the summary reports usage for: %s",
			s.DescribeVolumes("default", "nfsv-obs-06-run-writer"))
	}
	want := VolumeUsage{
		Source:         SourceKubeletSummary,
		Claim:          "nfsv-obs-06-run-claim",
		CapacityBytes:  1048576,
		UsedBytes:      10240,
		AvailableBytes: 1038336,
		At:             time.Date(2026, 9, 11, 10, 2, 3, 0, time.UTC),
	}
	if got.Source != want.Source || got.Claim != want.Claim || got.CapacityBytes != want.CapacityBytes ||
		got.UsedBytes != want.UsedBytes || got.AvailableBytes != want.AvailableBytes || !got.At.Equal(want.At) {
		t.Errorf("read %+v, want %+v", got, want)
	}

	if _, ok := s.VolumeUsageFor("default", "nfsv-obs-06-run-reader", "nfsv-obs-06-run-claim"); ok {
		t.Error("a claim was found under a pod with no entry in this summary, so a reading of one mount " +
			"would stand in for another pod's")
	}
	if _, ok := s.VolumeUsageFor("default", "nfsv-obs-06-run-writer", "some-other-claim"); ok {
		t.Error("a claim the pod does not mount was found, so the case would measure the wrong volume")
	}
}

// TestKubeletSummaryReportsAbsentUsage covers the shapes that must not read as
// a volume with nothing in it.
//
// This is the failure with no symptom. The byte counts are optional fields, and
// a driver that does not implement volume statistics produces an entry without
// them. Decoded into plain integers they come back as zero, the case then
// compares the workload's view against a published zero, and a deployment
// nobody can monitor for capacity reports a passing OBS-06.
//
// Steps:
//  1. Look up a claim whose entry carries no usedBytes.
//  2. Look up a claim whose entry carries no capacityBytes.
//  3. Look up a volume that belongs to no claim.
func TestKubeletSummaryReportsAbsentUsage(t *testing.T) {
	for name, doc := range map[string]string{
		"no usedBytes": `{"pods": [{"podRef": {"name": "p", "namespace": "default"},
			"volume": [{"time": "2026-09-11T10:00:00Z", "capacityBytes": 1048576, "name": "vol0",
			"pvcRef": {"name": "c", "namespace": "default"}}]}]}`,
		"no capacityBytes": `{"pods": [{"podRef": {"name": "p", "namespace": "default"},
			"volume": [{"time": "2026-09-11T10:00:00Z", "usedBytes": 10240, "name": "vol0",
			"pvcRef": {"name": "c", "namespace": "default"}}]}]}`,
		"no pvcRef": `{"pods": [{"podRef": {"name": "p", "namespace": "default"},
			"volume": [{"time": "2026-09-11T10:00:00Z", "capacityBytes": 1048576, "usedBytes": 10240,
			"name": "c"}]}]}`,
		"no volumes at all": `{"pods": [{"podRef": {"name": "p", "namespace": "default"}, "volume": []}]}`,
	} {
		s := decodeSummary(t, doc)
		if got, ok := s.VolumeUsageFor("default", "p", "c"); ok {
			t.Errorf("%s: read %+v instead of reporting that the control plane publishes no usage, "+
				"so a volume nobody can measure would pass as one nobody is filling", name, got)
		}
	}
}

// TestKubeletSummaryDescribeVolumes checks what a failure gets to say.
//
// "No usage for this claim" is actionable only next to what was published
// instead, since that is what tells an operator whether the driver reports
// nothing at all or reports everything except the claim.
//
// Steps:
//  1. Describe a pod whose entry carries volumes, claim-backed and not.
//  2. Describe a pod the summary says nothing about.
func TestKubeletSummaryDescribeVolumes(t *testing.T) {
	s := decodeSummary(t, summaryWithUsage)

	got := s.DescribeVolumes("default", "nfsv-obs-06-run-writer")
	for _, want := range []string{"2 volumes", "vol0", "kube-api-access-abcde", "nfsv-obs-06-run-claim", "no claim"} {
		if !strings.Contains(got, want) {
			t.Errorf("the description of what the kubelet published omits %q: %s", want, got)
		}
	}
	if missing := s.DescribeVolumes("default", "absent-pod"); !strings.Contains(missing, "absent-pod") {
		t.Errorf("the description of a pod with no entry does not name it: %s", missing)
	}
}

func decodeSummary(t *testing.T, doc string) *KubeletSummary {
	t.Helper()
	s, err := parseKubeletSummary([]byte(doc))
	if err != nil {
		t.Fatalf("decoding the summary: %v", err)
	}
	return s
}
