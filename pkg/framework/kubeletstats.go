package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/slo"
)

// The kubelet publishes what it knows about the pods on its node, including
// per-volume capacity and usage for the claims they mount. That is the control
// plane's answer to "how full is this volume", and it is the one an operator's
// monitoring reads, so OBS-06 compares it against what the workload itself
// sees.
//
// It is reached through the API server's node proxy, on the client the suite
// already holds and with the kubeconfig it already uses. Nothing is deployed to
// get it, no node address or node certificate is needed, and the privileged
// node agent is not involved: this is an ordinary GET.

// kubeletReadTimeout bounds one read of one kubelet.
//
// Individually bounded on purpose. A node whose kubelet has stopped answering
// must not be able to spend a case's whole budget, and one sick node must not
// starve a reading from a healthy one.
const kubeletReadTimeout = 30 * time.Second

// KubeletSummary is the part of the kubelet's stats summary the suite reads.
// The full document also carries node-level and container-level statistics;
// what is decoded here is what an assertion reads, and the rest is ignored
// rather than carried around unused.
type KubeletSummary struct {
	Pods []KubeletPodStats `json:"pods"`
}

// KubeletPodStats is one pod's entry in the summary.
type KubeletPodStats struct {
	PodRef  KubeletPodRef       `json:"podRef"`
	Volumes []KubeletVolumeStat `json:"volume"`
}

// KubeletPodRef identifies the pod an entry is about.
type KubeletPodRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// KubeletVolumeStat is one volume of one pod, as the kubelet last computed it.
//
// The three byte counts are pointers because the kubelet omits a field it does
// not have rather than sending a zero. Decoded into plain int64 they would come
// back as zero, and a volume whose usage the driver does not report would read
// as an empty volume: the case would then pass, having compared the workload's
// view against a number nobody published. That is precisely the finding OBS-06
// exists to report, so the absence has to survive the parse.
type KubeletVolumeStat struct {
	Time           metav1.Time    `json:"time"`
	AvailableBytes *int64         `json:"availableBytes"`
	CapacityBytes  *int64         `json:"capacityBytes"`
	UsedBytes      *int64         `json:"usedBytes"`
	Name           string         `json:"name"`
	PVCRef         *KubeletPVCRef `json:"pvcRef"`
}

// KubeletPVCRef is the claim a volume entry belongs to. A volume with no
// pvcRef is one of the pod's own volumes, a projected token or a configmap,
// and is not a claim anybody monitors for capacity.
type KubeletPVCRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// NodeSummary reads one node's stats summary through the API server's node
// proxy.
//
// A refusal is a blocked condition, never a finding about the deployment: the
// suite could not reach the source, so nothing was learned about what the
// cluster publishes. Every other failure is an ordinary error.
func NodeSummary(ctx context.Context, c *Client, node string) (*KubeletSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, kubeletReadTimeout)
	defer cancel()

	raw, err := c.Kube.CoreV1().RESTClient().Get().
		Resource("nodes").Name(node).SubResource("proxy").
		Suffix("stats", "summary").
		Do(ctx).Raw()
	if err != nil {
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return nil, Blockedf("reading the kubelet stats summary of node %s was refused: %v. "+
				"The suite needs get on nodes/proxy for the per-volume usage the control plane "+
				"publishes; without it nothing is known about what this deployment reports", node, err)
		}
		return nil, fmt.Errorf("reading the kubelet stats summary of node %s: %w", node, err)
	}
	s, err := parseKubeletSummary(raw)
	if err != nil {
		return nil, fmt.Errorf("decoding the kubelet stats summary of node %s: %w", node, err)
	}
	return s, nil
}

// parseKubeletSummary decodes what the kubelet served. Separate from the read
// so that the decoding, which is the part with a way to be silently wrong, can
// be tested against recorded documents on a workstation.
func parseKubeletSummary(raw []byte) (*KubeletSummary, error) {
	var s KubeletSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// VolumeUsageFor returns the kubelet's reading for one claim mounted by one
// pod, and whether it published one at all.
//
// Both halves of the identity are checked. Matching on the claim alone would
// pick up another pod's reading of the same RWX claim on the same node, which
// is a different mount with its own statistics.
func (s *KubeletSummary) VolumeUsageFor(namespace, pod, claim string) (VolumeUsage, bool) {
	for i := range s.Pods {
		p := &s.Pods[i]
		if p.PodRef.Namespace != namespace || p.PodRef.Name != pod {
			continue
		}
		for j := range p.Volumes {
			v := &p.Volumes[j]
			if v.PVCRef == nil || v.PVCRef.Name != claim {
				continue
			}
			if v.PVCRef.Namespace != "" && v.PVCRef.Namespace != namespace {
				continue
			}
			// A reading with no used bytes is not a reading. The kubelet has an
			// entry for the volume and no numbers in it, which is what a driver
			// that does not implement volume statistics produces.
			if v.UsedBytes == nil || v.CapacityBytes == nil {
				return VolumeUsage{}, false
			}
			return VolumeUsage{
				Source:         SourceKubeletSummary,
				Claim:          claim,
				CapacityBytes:  *v.CapacityBytes,
				UsedBytes:      *v.UsedBytes,
				AvailableBytes: derefBytes(v.AvailableBytes),
				At:             v.Time.Time,
			}, true
		}
	}
	return VolumeUsage{}, false
}

func derefBytes(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// DescribeVolumes says what the kubelet did publish for a pod, which is what a
// failure needs to say: "no usage for this claim" is actionable only next to
// the entries that were there.
func (s *KubeletSummary) DescribeVolumes(namespace, pod string) string {
	for i := range s.Pods {
		p := &s.Pods[i]
		if p.PodRef.Namespace != namespace || p.PodRef.Name != pod {
			continue
		}
		if len(p.Volumes) == 0 {
			return fmt.Sprintf("the kubelet has an entry for pod %s/%s with no volume statistics in it at all",
				namespace, pod)
		}
		var parts []string
		for j := range p.Volumes {
			v := &p.Volumes[j]
			claim := "no claim"
			if v.PVCRef != nil {
				claim = "claim " + v.PVCRef.Name
			}
			used := "no usedBytes"
			if v.UsedBytes != nil {
				used = fmt.Sprintf("%d bytes used", *v.UsedBytes)
			}
			parts = append(parts, fmt.Sprintf("%s (%s, %s)", v.Name, claim, used))
		}
		sort.Strings(parts)
		return fmt.Sprintf("the kubelet publishes %d volumes for pod %s/%s: %s",
			len(p.Volumes), namespace, pod, strings.Join(parts, ", "))
	}
	return fmt.Sprintf("the kubelet publishes no statistics for pod %s/%s at all", namespace, pod)
}

// The two ways the control plane can fail to answer "how full is this volume",
// which a case has to tell apart because they are reported to different people.
//
// No usage at all is a deployment finding: the CSI specification makes volume
// statistics an optional node capability, and a driver that does not implement
// them leaves every operator on that cluster without capacity monitoring. A
// reading that never catches up with the workload is the other half of the same
// question: a number nobody refreshes is not a measurement of anything.
//
// Neither is a blocked condition. Blocked is for a source the suite could not
// reach for its own reasons, and both of these are the cluster answering.
var (
	// ErrNoVolumeStats is the control plane publishing no usage for a claim.
	ErrNoVolumeStats = errors.New("the control plane publishes no usage for this volume")
	// ErrStaleVolumeStats is a reading that never caught up with the workload.
	ErrStaleVolumeStats = errors.New("the control plane's usage reading is older than the workload's")
)

// FreshKubeletUsage waits for a control plane reading of a claim whose own
// sample time is not older than notBefore, which is when the workload's own
// reading was taken.
//
// The wait is what makes the comparison meaningful rather than generous. The
// kubelet recomputes volume statistics on a period, so its answer is stale by
// up to that period; comparing against a sample taken before the workload read
// its own view would let a control plane that is a minute behind look like one
// that disagrees, or the other way round.
//
// It is also why an absent entry is waited on rather than failed immediately: a
// volume mounted a moment ago has not been through the kubelet's period yet,
// and a case that failed on the first read would name the driver for the
// kubelet's schedule.
func (f *Framework) FreshKubeletUsage(ctx context.Context, node, pod, claim string,
	notBefore time.Time, timeout time.Duration) (VolumeUsage, error) {
	var (
		latest VolumeUsage
		found  bool
		// refused stops the loop: a permission the kubeconfig does not have is
		// not going to arrive within the timeout, and polling on it would
		// spend the case's budget to report the same thing.
		refused error
	)
	waitErr := Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		s, err := NodeSummary(ctx, f.C, node)
		if err != nil {
			if IsBlocked(err) {
				refused = err
				return true, nil
			}
			return false, err
		}
		u, ok := s.VolumeUsageFor(Namespace, f.Name(pod), f.Name(claim))
		if !ok {
			return false, fmt.Errorf("%s", s.DescribeVolumes(Namespace, f.Name(pod)))
		}
		latest, found = u, true
		// The two timestamps come from different clocks, the kubelet's from its
		// node and the workload reading's from the workstation, so the guard
		// band that narrows every other two-clock comparison decides what
		// counts as "not older" here.
		if u.At.Before(notBefore.Add(-slo.ClockSkewGuard)) {
			return false, fmt.Errorf("the kubelet's sample is from %s, taken before the workload's reading at %s",
				u.At.UTC().Format(time.RFC3339), notBefore.UTC().Format(time.RFC3339))
		}
		return true, nil
	})
	switch {
	case refused != nil:
		return VolumeUsage{}, refused
	case waitErr != nil && !found:
		return VolumeUsage{}, fmt.Errorf("%w: the kubelet on %s published none for claim %s within %s: %v",
			ErrNoVolumeStats, node, f.Name(claim), timeout, waitErr)
	case waitErr != nil:
		return VolumeUsage{}, fmt.Errorf("%w: within %s the freshest reading from the kubelet on %s was %s: %v",
			ErrStaleVolumeStats, timeout, node, latest, waitErr)
	}
	return latest, nil
}
