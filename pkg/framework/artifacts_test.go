package framework

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mikebz/nfs-verification/pkg/env"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The failure bundle is only read after something has gone wrong, and a gap in
// it has no symptom: a log that could not be fetched and a pod that logged
// nothing leave the same directory behind, and so do a node whose dmesg timed
// out and a node whose kernel was quiet. Each of those invites a confident
// wrong reading of the failure (#118). They are caught here or on the day
// someone argues from an absence that was never observed.

// TestArtifactManifestNamesWhatWasNotCaptured checks the manifest tells every
// kind of row apart, and keeps evidence.txt's words for the ones it shares.
//
// Steps:
//  1. Render a complete row, a partial one, one that captured nothing, one
//     that was never attempted, and one with no file.
//  2. Assert each line says which it is, and that only the gaps are shouted.
func TestArtifactManifestNamesWhatWasNotCaptured(t *testing.T) {
	got := renderArtifacts([]artifactRow{
		{File: "client-pods/w-main.log", Source: "log of container main in pod default/w", Bytes: 412},
		{File: "client-pods/r-main.log", Source: "log of container main in pod default/r", Bytes: 90,
			Problem: "the stream stopped after 90 bytes: unexpected EOF"},
		{File: "dmesg-node-b.txt", Source: "dmesg on node node-b",
			Problem: "unreadable within 10s: context deadline exceeded"},
		{File: "client-pods/w-main.previous.log", Source: "previous log of container main in pod default/w",
			Skipped: "the container has not restarted, so there is no previous log"},
		{Source: "the node agent", Problem: "not reached, so no node was inspected: forbidden"},
	})
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("the manifest has %d lines, want two of preamble and five of rows:\n%s", len(lines), got)
	}
	for _, want := range []string{
		"client-pods/w-main.log\tlog of container main in pod default/w\t412 bytes\tcomplete",
		"90 bytes\tPARTIAL, this is what arrived before it stopped: the stream stopped after 90 bytes",
		"dmesg-node-b.txt\tdmesg on node node-b\t0 bytes\tNOT CAPTURED: unreadable within 10s",
		"0 bytes\tnot attempted: the container has not restarted",
		"-\tthe node agent\t0 bytes\tNOT CAPTURED: not reached",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the manifest does not say %q:\n%s", want, got)
		}
	}
}

// TestArtifactManifestKeepsAMultiLineErrorOnOneRow exists because the text in a
// row is not all the harness's own. A node-agent failure carries the command's
// combined output, and one raw newline or tab in it would split the row, or
// shift its columns, so that the tail of an error reads as an artifact.
//
// Steps:
//  1. Render one failed row whose problem has a newline, a tab, a carriage
//     return and a backslash in it, the way a failed nsenter reports.
//  2. Assert the manifest is the preamble and one row of exactly four
//     columns, and that each character arrived escaped.
func TestArtifactManifestKeepsAMultiLineErrorOnOneRow(t *testing.T) {
	got := renderArtifacts([]artifactRow{{
		File: "dmesg-node-b.txt", Source: "dmesg on node node-b",
		Problem: "node node-b: command terminated with exit code 1: nsenter: cannot open\r\n\tC:\\proc\nsecond line",
	}})
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("the manifest has %d lines, want two of preamble and one row:\n%s", len(lines), got)
	}
	if cols := strings.Split(lines[2], "\t"); len(cols) != 4 {
		t.Errorf("the row has %d columns, want 4: %q", len(cols), lines[2])
	}
	if want := `cannot open\r\n\tC:\\proc\nsecond line`; !strings.HasSuffix(lines[2], want) {
		t.Errorf("the row does not end with the escaped error %q: %q", want, lines[2])
	}
}

// TestNodeReadMarksAFailedDmesgLikeAFailedMountsRead covers the asymmetry the
// issue was filed about: a failed /proc/mounts read left a note, a failed dmesg
// left nothing, and the node a chaos failure is about looked uninspected.
//
// Steps:
//  1. Record a failed /proc/mounts read, a failed dmesg and a good dmesg.
//  2. Assert both failures leave a placeholder file of the same shape and a
//     NOT CAPTURED row counted as a gap, and the good read is complete.
func TestNodeReadMarksAFailedDmesgLikeAFailedMountsRead(t *testing.T) {
	b := &bundle{dir: t.TempDir()}
	timeout := errors.New("context deadline exceeded")
	b.nodeRead("proc-mounts-a.txt", "/proc/mounts on node a", "partial output", timeout)
	b.nodeRead("dmesg-a.txt", "dmesg on node a", "", timeout)
	b.nodeRead("dmesg-b.txt", "dmesg on node b", "[    0.000000] Linux version\n", nil)

	for _, file := range []string{"proc-mounts-a.txt", "dmesg-a.txt"} {
		data, err := os.ReadFile(filepath.Join(b.dir, file))
		if err != nil {
			t.Fatalf("a failed read left no %s: %v", file, err)
		}
		if want := "unreadable within 10s: context deadline exceeded\n"; string(data) != want {
			t.Errorf("%s holds %q, want the placeholder %q", file, data, want)
		}
	}
	if got := len(b.gaps()); got != 2 {
		t.Errorf("%d gaps recorded, want the two failed reads: %v", got, b.gaps())
	}
	if r := b.rows[2]; r.Problem != "" || r.Bytes != int64(len("[    0.000000] Linux version\n")) {
		t.Errorf("a good dmesg was recorded as %+v, want complete with its byte count", r)
	}
	for _, r := range b.rows[:2] {
		if r.Bytes != 0 {
			t.Errorf("%s counts %d bytes, but a placeholder is not what was asked for", r.File, r.Bytes)
		}
	}
}

// TestCollectArtifactsRecordsEveryGap drives the whole collector against a fake
// cluster in which some reads fail, and checks the manifest and the returned
// error both account for them.
//
// Steps:
//  1. Build a client pod with a container that never restarted, one that did,
//     and one with no status yet, and make the first one's log impossible to
//     write by putting a directory where it would land.
//  2. Point server discovery at a namespace whose pod list fails, and leave
//     the node agent capability off.
//  3. Collect, and assert each artifact has the row it earned: complete,
//     NOT CAPTURED, or not attempted.
//  4. Assert the returned error names the two gaps and nothing that was not
//     one.
func TestCollectArtifactsRecordsEveryGap(t *testing.T) {
	originalDir, originalRun := cfg.ArtifactsDir, cfg.RunID
	defer func() { cfg.ArtifactsDir, cfg.RunID = originalDir, originalRun }()
	cfg.ArtifactsDir = t.TempDir()
	cfg.RunID = "unit-118"

	kube := fake.NewSimpleClientset()
	e := &env.Environment{Servers: []env.ServerInfo{{Namespace: "nfs-server", Pod: "nfs-0"}}}
	f, _ := NewSystem(&Client{Kube: kube}, e, Capabilities{}, "DATA-02")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: Namespace, Labels: f.Labels()},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}, {Name: "sidecar"}, {Name: "late"}}},
		// "late" has no status yet, as on a pending pod: whether it restarted
		// is unknown, not zero.
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "main", RestartCount: 0}, {Name: "sidecar", RestartCount: 2},
		}},
	}
	if err := kube.Tracker().Add(pod); err != nil {
		t.Fatal(err)
	}
	kube.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "nfs-server" {
			return true, nil, errors.New("pods is forbidden")
		}
		return false, nil, nil
	})
	dir := CaseDir("DATA-02")
	if err := os.MkdirAll(filepath.Join(dir, "client-pods", "writer-main.log"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := f.CollectArtifacts(t.Context())
	manifest, readErr := os.ReadFile(filepath.Join(dir, artifactManifest))
	if readErr != nil {
		t.Fatalf("no manifest was written: %v", readErr)
	}
	got := string(manifest)
	for _, want := range []string{
		"environment.json\tthe run's environment record\t",
		"fault-timeline.json\tthe faults this case injected\t",
		"client-pods/writer.json\tpod default/writer\t",
		"client-pods/writer-main.log\tlog of container main in pod default/writer\t0 bytes\tNOT CAPTURED: streamed but not written",
		"client-pods/writer-main.previous.log\tprevious log of container main in pod default/writer\t0 bytes\tnot attempted: the container has not restarted",
		"client-pods/writer-sidecar.log\tlog of container sidecar in pod default/writer\t9 bytes\tcomplete",
		// Attempted, because an unreported count is not a zero one. The fake
		// answers every log request, so complete is what proves it was asked.
		"client-pods/writer-late.previous.log\tprevious log of container late in pod default/writer\t9 bytes\tcomplete",
		"client-pods/writer-sidecar.previous.log\tprevious log of container sidecar in pod default/writer\t9 bytes\tcomplete",
		"server-pods/\tserver pods in namespace nfs-server matching \t0 bytes\tNOT CAPTURED: not listed: pods is forbidden",
		"events-default.txt\tKubernetes Events in namespace default\t0 bytes\tcomplete",
		"events-nfs-server.txt\tKubernetes Events in namespace nfs-server\t0 bytes\tcomplete",
		"-\t/proc/mounts and dmesg from every node\t0 bytes\tnot attempted: this cluster has no node agent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the manifest does not say %q:\n%s", want, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "environment.json") || strings.HasPrefix(line, "fault-timeline.json") ||
			strings.HasPrefix(line, "client-pods/writer.json") {
			if !strings.HasSuffix(line, "\tcomplete") {
				t.Errorf("an artifact that was written is not marked complete: %q", line)
			}
		}
	}

	if err == nil {
		t.Fatal("two artifacts were not captured and CollectArtifacts returned no error")
	}
	for _, want := range []string{"writer-main.log", "server-pods/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name the gap %q: %v", want, err)
		}
	}
	for _, not := range []string{"previous.log", "node agent"} {
		if strings.Contains(err.Error(), not) {
			t.Errorf("the error reports %q, which was deliberately not attempted, as a gap: %v", not, err)
		}
	}
}
