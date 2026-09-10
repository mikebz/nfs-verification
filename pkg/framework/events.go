package framework

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// Events are the channel a cluster uses to tell an operator that something did
// not work. A failure the operator can only find by reading kubelet logs on the
// node is, for practical purposes, a silent failure, which is why OBS-04
// asserts on the Event rather than on the pod's phase.

// PodEvents returns the events recorded against a pod, oldest first.
func (f *Framework) PodEvents(ctx context.Context, pod string) ([]corev1.Event, error) {
	name := f.Name(pod)
	sel := fields.AndSelectors(
		fields.OneTermEqualSelector("involvedObject.name", name),
		fields.OneTermEqualSelector("involvedObject.kind", "Pod"),
	).String()
	list, err := f.C.Kube.CoreV1().Events(Namespace).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, err
	}
	out := list.Items
	sort.Slice(out, func(i, j int) bool { return eventTime(&out[i]).Before(eventTime(&out[j])) })
	return out, nil
}

// eventTime is when an event last happened. Kubernetes fills these fields
// inconsistently across versions and event sources, so all three are tried
// rather than assuming the one this cluster happens to use.
func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		return e.Series.LastObservedTime.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

// WaitPodEvent waits for an event on a pod that match accepts, and returns it.
// The timeout error names every event seen, because "no matching event" with
// nothing else said is impossible to act on.
func (f *Framework) WaitPodEvent(ctx context.Context, pod string, timeout time.Duration, match func(corev1.Event) bool) (corev1.Event, error) {
	var found corev1.Event
	var seen []corev1.Event
	err := Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		evs, err := f.PodEvents(ctx, pod)
		if err != nil {
			return false, err
		}
		seen = evs
		for _, e := range evs {
			if match(e) {
				found = e
				return true, nil
			}
		}
		return false, fmt.Errorf("no matching event among %d on %s", len(evs), f.Name(pod))
	})
	if err != nil {
		return corev1.Event{}, fmt.Errorf("%w; events on %s were: %s", err, f.Name(pod), DescribeEvents(seen))
	}
	return found, nil
}

// DescribeEvents renders events for a failure message.
func DescribeEvents(evs []corev1.Event) string {
	if len(evs) == 0 {
		return "(none)"
	}
	var parts []string
	for i := range evs {
		parts = append(parts, fmt.Sprintf("[%s/%s] %s", evs[i].Type, evs[i].Reason, strings.TrimSpace(evs[i].Message)))
	}
	return strings.Join(parts, " | ")
}
