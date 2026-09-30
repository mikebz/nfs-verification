package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nodeInspectTimeout bounds one node's share of the artifact bundle.
const nodeInspectTimeout = 10 * time.Second

// artifactManifest names the index of what the failure bundle tried to
// collect. It is to CollectArtifacts what evidence.txt is to the files a case
// named: the half a reader needs in order to trust the other files.
const artifactManifest = "artifacts.txt"

// CollectArtifacts writes the triage bundle for a failed case: the environment
// record, pod logs, Kubernetes Events, server logs, /proc/mounts and dmesg from
// every involved node, and the fault timeline. A failure filed without this
// bundle will be closed as unreproducible.
//
// Every artifact it tries gets a line in artifacts.txt, and so does every one
// it failed to get, with the reason (#118). A gap nobody records reads as an
// answer: a node whose dmesg timed out, which is the node a chaos failure is
// about, otherwise looks the same as a node whose kernel had nothing to say,
// and a pod log that could not be fetched looks the same as a pod that logged
// nothing. The returned error lists the same gaps, so the run log says the
// bundle is short without anyone having to open it.
func (f *Framework) CollectArtifacts(ctx context.Context) error {
	dir := CaseDir(f.CaseID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b := &bundle{dir: dir}

	f.dumpEnvironment(b)
	f.dumpTimeline(b)
	f.dumpPods(ctx, b, Namespace, f.Selector(), "client")
	if ns := f.Env.ServerNamespace(); ns != "" {
		f.dumpPods(ctx, b, ns, Cfg().ServerSelector, "server")
	} else {
		b.skip("server-pods/", "NFS server pods and their logs",
			"discovery found no server pods, so there were none to read")
	}
	f.dumpEvents(ctx, b)
	if f.Caps.NodeAgent {
		f.dumpNodeState(ctx, b)
	} else {
		b.skip("", "/proc/mounts and dmesg from every node",
			"this cluster has no node agent (capability NodeAgent), so no node was inspected")
	}

	gaps := b.gaps()
	if err := os.WriteFile(filepath.Join(dir, artifactManifest), []byte(renderArtifacts(b.rows)), 0o644); err != nil {
		gaps = append(gaps, fmt.Sprintf("%s: %v", artifactManifest, err))
	}
	if len(gaps) > 0 {
		return fmt.Errorf("artifact collection in %s had gaps: %s", dir, strings.Join(gaps, "; "))
	}
	if f.T != nil {
		f.T.Logf("artifacts written to %s", dir)
	}
	return nil
}

// bundle is a failure bundle being written, together with the account of it
// that becomes artifacts.txt. Collection is sequential, so it needs no lock.
type bundle struct {
	dir  string
	rows []artifactRow
}

// artifactRow is what the manifest says about one thing CollectArtifacts tried
// to collect, or knowingly did not.
type artifactRow struct {
	// File is relative to the case directory. A trailing slash names a
	// directory, and empty means the attempt failed before it had a file.
	File   string
	Source string
	// Bytes counts what was collected, never a placeholder written in its
	// place.
	Bytes int64
	// Problem is why the file is not the whole of what was asked for, empty
	// when it is; Bytes says whether anything arrived in spite of it. A row
	// with a problem is a gap, and goes in the error CollectArtifacts returns.
	Problem string
	// Skipped is why nothing was attempted: the artifact is known not to
	// exist, or this cluster cannot provide it. Not a gap in the collection,
	// so not an error, but written down so that its absence is not mistaken
	// for a clean result.
	Skipped string
}

// write puts one collected artifact in the bundle and records it.
func (b *bundle) write(file, source string, data []byte) {
	row := artifactRow{File: file, Source: source}
	if err := os.WriteFile(filepath.Join(b.dir, file), data, 0o644); err != nil {
		row.Problem = fmt.Sprintf("collected but not written: %v", err)
	} else {
		row.Bytes = int64(len(data))
	}
	b.rows = append(b.rows, row)
}

// fail records an artifact that was tried and not got.
func (b *bundle) fail(file, source, problem string) {
	b.rows = append(b.rows, artifactRow{File: file, Source: source, Problem: problem})
}

// skip records an artifact that was deliberately not tried, and why.
func (b *bundle) skip(file, source, reason string) {
	b.rows = append(b.rows, artifactRow{File: file, Source: source, Skipped: reason})
}

// nodeRead records one read from a node. /proc/mounts and dmesg both go
// through it, so that a failed read of either leaves the same placeholder file
// saying so: a missing dmesg once left no file at all where a missing
// /proc/mounts left a note, and the node a chaos failure cares about looked
// like one that was never inspected (#118).
func (b *bundle) nodeRead(file, source, out string, err error) {
	if err == nil {
		b.write(file, source, []byte(out))
		return
	}
	problem := fmt.Sprintf("unreadable within %s: %v", nodeInspectTimeout, err)
	if werr := os.WriteFile(filepath.Join(b.dir, file), []byte(problem+"\n"), 0o644); werr != nil {
		problem += fmt.Sprintf("; the placeholder saying so was not written either: %v", werr)
	}
	b.fail(file, source, problem)
}

// gaps lists every row with a problem, one line each.
func (b *bundle) gaps() []string {
	var out []string
	for _, r := range b.rows {
		if r.Problem != "" {
			out = append(out, fmt.Sprintf("%s (%s): %s", manifestFile(r.File), r.Source, r.Problem))
		}
	}
	return out
}

// manifestFile is how a row's file appears in the manifest; a row with no file
// still gets a column, so the lines stay aligned for a reader and for cut.
func manifestFile(file string) string {
	if file == "" {
		return "-"
	}
	return file
}

// renderArtifacts writes the manifest: every artifact tried, where it came
// from, how much arrived, and what was not got and why.
//
// The words are evidence.txt's, so one reading serves both files: complete,
// PARTIAL and NOT CAPTURED, the last two capitalised because they are the lines
// a reader must not miss. "not attempted" is the one addition, for an artifact
// the collector knew better than to ask for, such as the previous log of a
// container that never restarted. It is not a gap, so it is not shouted, but it
// is written down, because silence about it is exactly the ambiguity this file
// exists to remove.
func renderArtifacts(rows []artifactRow) string {
	var sb strings.Builder
	sb.WriteString("# what the failure bundle tried to collect, including what it could not get and why\n")
	sb.WriteString("# one line per artifact: file, source, bytes collected, status\n")
	for _, r := range rows {
		status := "complete"
		switch {
		case r.Problem != "" && r.Bytes > 0:
			status = "PARTIAL, this is what arrived before it stopped: " + r.Problem
		case r.Problem != "":
			status = "NOT CAPTURED: " + r.Problem
		case r.Skipped != "":
			status = "not attempted: " + r.Skipped
		}
		fmt.Fprintf(&sb, "%s\t%s\t%d bytes\t%s\n",
			manifestField(manifestFile(r.File)), manifestField(r.Source), r.Bytes, manifestField(status))
	}
	return sb.String()
}

// manifestField escapes one text column of the manifest, so one artifact stays
// on one line in four columns. The text in a row is not all the harness's own:
// a node-agent failure carries whatever the command printed, several lines of
// it, and one raw newline would split a row in two and leave the second half
// reading as an artifact nobody collected. The backslash is escaped too, so
// that an escape sequence in the manifest always means one of these characters
// and never itself.
func manifestField(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`).Replace(s)
}

// dumpEnvironment writes the run's environment record into the case bundle.
func (f *Framework) dumpEnvironment(b *bundle) {
	const file, source = "environment.json", "the run's environment record"
	if f.Env == nil {
		b.skip(file, source, "this fixture carries no environment record")
		return
	}
	path, err := f.Env.Write(b.dir)
	if err != nil {
		b.fail(file, source, fmt.Sprintf("not written: %v", err))
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		b.fail(file, source, fmt.Sprintf("written but not found afterwards: %v", err))
		return
	}
	b.rows = append(b.rows, artifactRow{File: file, Source: source, Bytes: info.Size()})
}

func (f *Framework) dumpPods(ctx context.Context, b *bundle, ns, selector, kind string) {
	sub := kind + "-pods"
	source := fmt.Sprintf("%s pods in namespace %s matching %s", kind, ns, selector)
	pods, err := f.C.Kube.CoreV1().Pods(ns).List(ctx, ListOptions(selector))
	if err != nil {
		b.fail(sub+"/", source, fmt.Sprintf("not listed: %v", err))
		return
	}
	if err := os.MkdirAll(filepath.Join(b.dir, sub), 0o755); err != nil {
		b.fail(sub+"/", source, fmt.Sprintf("listed but the directory was not created: %v", err))
		return
	}
	if len(pods.Items) == 0 {
		b.skip(sub+"/", source, "no pod matched")
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		file, podSource := sub+"/"+p.Name+".json", fmt.Sprintf("pod %s/%s", ns, p.Name)
		if data, err := json.MarshalIndent(p, "", "  "); err != nil {
			b.fail(file, podSource, fmt.Sprintf("not encoded: %v", err))
		} else {
			b.write(file, podSource, data)
		}
		restarts := map[string]int32{}
		for _, s := range p.Status.ContainerStatuses {
			restarts[s.Name] = s.RestartCount
		}
		for _, c := range p.Spec.Containers {
			f.writeLogs(ctx, b, sub, ns, p.Name, c.Name, false)
			// Previous-container logs are where a crash actually shows up. A
			// container that has not restarted has none, and asking anyway
			// would put a refusal against every healthy pod in the manifest,
			// burying the gaps that matter. The count is the one in the pod
			// record beside the logs, so the two agree. Only a count the pod
			// actually reported earns the skip: a container with no status
			// yet, as on a pending pod, is unknown rather than unrestarted,
			// so its previous log is asked for and the answer recorded.
			if n, reported := restarts[c.Name]; reported && n == 0 {
				b.skip(logFile(sub, p.Name, c.Name, true), logSource(ns, p.Name, c.Name, true),
					"the container has not restarted, so there is no previous log")
				continue
			}
			f.writeLogs(ctx, b, sub, ns, p.Name, c.Name, true)
		}
	}
}

// logFile names one container's log in the bundle.
//
// The separator is an underscore because neither name can contain one: a pod
// name is a DNS-1123 subdomain and a container name a DNS-1123 label. A hyphen
// was used before, and pod a-b with container c landed on the same file as pod
// a with container b-c, the second log silently replacing the first while the
// manifest claimed both. A container name cannot contain a dot either, so the
// .previous suffix cannot be mistaken for part of the name.
func logFile(sub, pod, container string, previous bool) string {
	if previous {
		return sub + "/" + pod + "_" + container + ".previous.log"
	}
	return sub + "/" + pod + "_" + container + ".log"
}

// logSource says in the manifest which log a file is.
func logSource(ns, pod, container string, previous bool) string {
	if previous {
		return fmt.Sprintf("previous log of container %s in pod %s/%s", container, ns, pod)
	}
	return fmt.Sprintf("log of container %s in pod %s/%s", container, ns, pod)
}

// writeLogs copies one container's log into the bundle and records what
// arrived.
func (f *Framework) writeLogs(ctx context.Context, b *bundle, sub, ns, pod, container string, previous bool) {
	req := f.C.Kube.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: previous})
	rc, err := req.Stream(ctx)
	b.saveLog(logFile(sub, pod, container, previous), logSource(ns, pod, container, previous), rc, err)
}

// saveLog writes one log stream into the bundle and records what landed. It
// is separate from the request so that a stream failing partway can be tested
// without a cluster.
//
// A log that could not be opened leaves no file, so that nothing in the bundle
// poses as the container's output; the manifest says why instead. A stream
// that stops partway keeps what it delivered and is marked PARTIAL, because
// the lines before the break are the ones nearest whatever broke it.
func (b *bundle) saveLog(file, source string, rc io.ReadCloser, openErr error) {
	row := artifactRow{File: file, Source: source}
	defer func() { b.rows = append(b.rows, row) }()
	if openErr != nil {
		row.Problem = fmt.Sprintf("not streamed: %v", openErr)
		return
	}
	out, err := os.Create(filepath.Join(b.dir, file))
	if err != nil {
		row.Problem = errors.Join(fmt.Errorf("streamed but not written: %w", err), rc.Close()).Error()
		return
	}
	n, err := io.Copy(out, rc)
	if err != nil {
		err = fmt.Errorf("the stream stopped after %d bytes: %w", n, err)
	}
	row.Bytes = n
	if err := errors.Join(err, out.Close(), rc.Close()); err != nil {
		row.Problem = err.Error()
	}
}

func (f *Framework) dumpEvents(ctx context.Context, b *bundle) {
	namespaces := []string{Namespace}
	if ns := f.Env.ServerNamespace(); ns != "" && ns != Namespace {
		namespaces = append(namespaces, ns)
	}
	for _, ns := range namespaces {
		// One namespace failing costs that namespace's file, not the next one.
		file, source := "events-"+ns+".txt", "Kubernetes Events in namespace "+ns
		evs, err := f.C.Kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			b.fail(file, source, fmt.Sprintf("not listed: %v", err))
			continue
		}
		var sb strings.Builder
		for _, e := range evs.Items {
			fmt.Fprintf(&sb, "%s\t%s\t%s\t%s/%s\t%s\n",
				e.LastTimestamp.Time.Format("15:04:05"), e.Type, e.Reason,
				e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message)
		}
		b.write(file, source, []byte(sb.String()))
	}
}

func (f *Framework) dumpNodeState(ctx context.Context, b *bundle) {
	const source = "the node agent, which reads /proc/mounts and dmesg from every node"
	agent, err := NodeAgent(ctx, f.C)
	if err != nil {
		b.fail("", source, fmt.Sprintf("not reached, so no node was inspected: %v", err))
		return
	}
	inspectNodes(ctx, b, agent, source)
}

// inspectNodes reads /proc/mounts and dmesg from every node with a running
// agent pod. Separate from dumpNodeState, which goes through the process-wide
// agent, so that it can be tested against an agent over a fake cluster.
func inspectNodes(ctx context.Context, b *bundle, agent *Agent, source string) {
	nodes, err := agent.Nodes(ctx)
	if err != nil {
		b.fail("", source, fmt.Sprintf("nodes not listed, so no node was inspected: %v", err))
		return
	}
	if len(nodes) == 0 {
		// The capability was established at preflight, and Nodes lists only
		// agent pods that are Running now, so an agent disrupted since then
		// answers with nothing. Nothing is not a clean result: no node was
		// read, and the manifest has to say so.
		b.fail("", source, "no agent pod is running on any node, so no node was inspected")
		return
	}
	for _, n := range nodes {
		// One node at a time, each on its own short clock. A node with a wedged
		// mount blocks on cat /proc/mounts, and the whole point of the bundle is
		// that it still contains the other nodes when that happens.
		nodeCtx, cancel := context.WithTimeout(ctx, nodeInspectTimeout)
		mounts, err := agent.ReadFile(nodeCtx, n, "/proc/mounts")
		b.nodeRead("proc-mounts-"+n+".txt", "/proc/mounts on node "+n, mounts, err)
		dmesg, err := agent.Dmesg(nodeCtx, n)
		b.nodeRead("dmesg-"+n+".txt", "dmesg on node "+n, dmesg, err)
		cancel()
	}
}

// WriteArtifact puts a file in this case's bundle.
//
// CollectArtifacts runs only when a case fails. This is for an observation a
// case wants recorded whether it passed or not: a verdict table, a mount line,
// a lock table. A pass that took a different path from the last pass is worth
// seeing, and by teardown the pods that could answer are gone.
func (f *Framework) WriteArtifact(name string, content []byte) error {
	dir := CaseDir(f.CaseID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), content, 0o644)
}

// RecordNodeLocks writes each node's own lock table into the bundle, labelled
// by the moment it was taken.
//
// This is the *client's* belief, not the server's, and the difference is the
// point: a lock the client thinks it holds and the server has forgotten is
// visible as a line here with no matching refusal at the server, and reading
// only one side cannot see it.
func (f *Framework) RecordNodeLocks(ctx context.Context, label string, nodes ...string) error {
	agent, err := NodeAgent(ctx, f.C)
	if err != nil {
		return err
	}
	var sb strings.Builder
	for _, n := range nodes {
		fmt.Fprintf(&sb, "# %s, node %s\n", label, n)
		locks, err := agent.Locks(ctx, n)
		if err != nil {
			fmt.Fprintf(&sb, "unreadable: %v\n\n", err)
			continue
		}
		for _, l := range locks {
			fmt.Fprintf(&sb, "%s\n", l)
		}
		sb.WriteString("\n")
	}
	return f.WriteArtifact("proc-locks-"+label+".txt", []byte(sb.String()))
}

// FaultEvent is one entry in the injected-fault timeline.
type FaultEvent struct {
	At     string `json:"at"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail,omitempty"`
}

// RecordFault appends to the case timeline. Chaos helpers call it so that the
// bundle says exactly what was done and when.
func (f *Framework) RecordFault(e FaultEvent) {
	f.state.mu.Lock()
	f.state.faults = append(f.state.faults, e)
	f.state.mu.Unlock()
	if f.T != nil {
		f.T.Logf("fault: %s %s %s", e.Action, e.Target, e.Detail)
	}
}

func (f *Framework) dumpTimeline(b *bundle) {
	const file, source = "fault-timeline.json", "the faults this case injected"
	f.state.mu.Lock()
	faults := append([]FaultEvent(nil), f.state.faults...)
	f.state.mu.Unlock()
	data, err := json.MarshalIndent(faults, "", "  ")
	if err != nil {
		b.fail(file, source, fmt.Sprintf("not encoded: %v", err))
		return
	}
	b.write(file, source, data)
}
