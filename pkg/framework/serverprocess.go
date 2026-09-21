package framework

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Naming the process that serves NFS is the input to every in-place kill, and
// getting it wrong is the most expensive mistake in this repository: the signal
// is delivered by name across the whole node, so a wrong name SIGKILLs
// something that has nothing to do with NFS and takes a node out of service.
//
// The name is therefore observed rather than inferred. The one fact that
// identifies the server without reference to any implementation is that it
// holds the listening socket on the NFS port, so the question is answered by
// joining a socket to the process holding it, on the socket's inode.
//
// That join is read from two places, because neither can answer alone:
//
//   - The socket is named inside the server pod. Which socket is the pod's is a
//     question about its network namespace, and the node's socket tables do not
//     contain it.
//   - The process is named on the node, through the node agent. Inside the pod
//     the server's file descriptors are unreadable without CAP_SYS_PTRACE,
//     which a container does not have, on every server observed that had been
//     running for more than a few minutes; the node agent is privileged and
//     reads them (F-027 in docs/findings.md). The pid it reports is also the
//     node's, which is the namespace the kill is matched in.
//
// What this replaces is the container's declared command, which is a proxy for
// the same question and answers it wrongly on a supervised server. The
// reference deployment's PodSpec declares no command at all, and its PID 1 is
// nfs-provisioner, which starts and supervises ganesha.nfsd: a harness that
// took the command would aim CHAOS-01 at the supervisor. See F-026 in
// docs/findings.md.

// serverProcessProbeTimeout bounds one read. The pod's containers are tried in
// turn and the node is read once, each on its own clock, so one container that
// does not answer cannot spend the whole budget and leave nothing for the rest.
const serverProcessProbeTimeout = 20 * time.Second

// NFSPort is the port an NFSv4 server listens on. Fixed by IANA and by
// RFC 8881 section 2.9.4, and not a thing a deployment varies.
const NFSPort uint16 = 2049

// ServerProcess is the process serving NFS in a server pod, with the evidence
// for the claim. The evidence travels with the name because the name alone is
// about to be handed to a node-wide kill, and a reviewer reading the fault
// timeline afterwards has to be able to check it.
type ServerProcess struct {
	// Name is the process name a node-level match will see.
	Name string
	// PID is the process id on the node, in the same namespace the kill is
	// matched in. It is evidence rather than a handle: it changes every time
	// the process restarts, which is what CHAOS-01 is watching for.
	PID int
	// Node is where that pid lives.
	Node string
	// Container is the container whose socket table named the socket.
	Container string
	// Socket is the listening socket the process holds, rendered.
	Socket string
	// Inode is that socket's inode, which is what tied the two together.
	Inode string
}

// String renders the observation the way a fault record wants it.
func (p ServerProcess) String() string {
	return fmt.Sprintf("%s (pid %d on %s) holds the listening socket %s, inode %s, seen from container %s",
		p.Name, p.PID, p.Node, p.Socket, p.Inode, p.Container)
}

// DiscoverServerProcess names the process listening on port in a server pod.
//
// The pod says which socket is listening and the node says who holds it; see
// the comment at the top of this file for why it takes both. The failure names
// which half could not be read, because they call for different actions: no
// listener at all is discovery pointed at the wrong pod, while a listener no
// process on the node holds is a socket held by something outside this node.
func DiscoverServerProcess(ctx context.Context, c *Client, agent *Agent, namespace, pod, node string, containers []string, port uint16) (ServerProcess, error) {
	if agent == nil {
		return ServerProcess{}, fmt.Errorf("no node agent, so the process holding the listening socket in "+
			"%s/%s cannot be named", namespace, pod)
	}
	if node == "" {
		return ServerProcess{}, fmt.Errorf("server pod %s/%s is not scheduled to a node", namespace, pod)
	}
	inodes, container, err := listeningSocket(ctx, c, namespace, pod, containers, port)
	if err != nil {
		return ServerProcess{}, err
	}

	scanCtx, cancel := context.WithTimeout(ctx, serverProcessProbeTimeout)
	defer cancel()
	out, err := agent.RunScript(scanCtx, node, "nfs-listener.sh",
		"listener-"+strings.ToLower(Cfg().RunID), append([]string{"/proc"}, inodes...)...)
	if err != nil {
		return ServerProcess{}, fmt.Errorf("scanning %s for the holder of socket %s: %w",
			node, strings.Join(inodes, ","), err)
	}
	p, err := pickHolder(parseListenerFacts(out), inodes)
	if err != nil {
		return ServerProcess{}, fmt.Errorf("port %d is listening in %s/%s on inode %s, but %s: %w",
			port, namespace, pod, strings.Join(inodes, ","), node, err)
	}
	p.Node, p.Container = node, container
	p.Socket = fmt.Sprintf("port %d", port)
	return p, nil
}

// listeningSocket reads the server pod's own socket tables and returns the
// inodes of the sockets listening on port, along with the container that
// answered.
//
// Every container in a pod shares one network namespace, so the first that
// answers is as good as any; they are tried in turn only because one of them
// may be unable to run a shell or read /proc at all.
func listeningSocket(ctx context.Context, c *Client, namespace, pod string, containers []string, port uint16) ([]string, string, error) {
	if len(containers) == 0 {
		return nil, "", fmt.Errorf("server pod %s/%s has no containers to look in", namespace, pod)
	}
	script, err := RunScript("nfs-listener.sh", "listener-"+strings.ToLower(Cfg().RunID))
	if err != nil {
		return nil, "", err
	}
	var why []string
	for _, container := range containers {
		probeCtx, cancel := context.WithTimeout(ctx, serverProcessProbeTimeout)
		r := c.Sh(probeCtx, namespace, pod, container, script)
		cancel()
		if r.Err != nil {
			why = append(why, fmt.Sprintf("%s: %v: %s", container, r.Err, strings.TrimSpace(truncate(r.Combined(), 200))))
			continue
		}
		inodes, err := listeningInodes(parseListenerFacts(r.Stdout), port)
		if err != nil {
			why = append(why, container+": "+err.Error())
			continue
		}
		return inodes, container, nil
	}
	return nil, "", fmt.Errorf("cannot see a socket listening on port %d in %s/%s: %s",
		port, namespace, pod, strings.Join(why, "; "))
}

// listenerFacts is what scripts/nfs-listener.sh reported: socket tables, and
// which process holds which socket. It holds no conclusions.
type listenerFacts struct {
	// tables maps a socket table's path to its contents.
	tables map[string]string
	// tableErrors holds the tables that exist and could not be read.
	tableErrors []string
	// comm and argv0 map a pid to what the kernel says it is running.
	comm  map[int]string
	argv0 map[int]string
	// holders maps a socket inode to the pids holding it.
	holders map[string][]int
	// noReadlink records an image that cannot resolve an fd at all, which is
	// not the same as a socket no process holds.
	noReadlink bool
	// complete records the reader's end marker. An exec that returns success
	// with truncated output otherwise reads as a server holding nothing, which
	// is F-011 in docs/findings.md.
	complete bool
}

// parseListenerFacts reads the reader's output. Unknown markers are ignored
// rather than refused: a newer reader against an older parser should degrade to
// a named failure from pickListener, not to a parse error nobody can action.
func parseListenerFacts(out string) listenerFacts {
	f := listenerFacts{
		tables:  map[string]string{},
		comm:    map[int]string{},
		argv0:   map[int]string{},
		holders: map[string][]int{},
	}
	var table string
	var body []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case table != "" && trimmed == "==ENDTABLE":
			f.tables[table] = strings.Join(body, "\n")
			table, body = "", nil
		case table != "":
			body = append(body, line)
		case strings.HasPrefix(trimmed, "==TABLE "):
			table, body = strings.TrimSpace(strings.TrimPrefix(trimmed, "==TABLE ")), nil
		case strings.HasPrefix(trimmed, "==TABLEERROR "):
			f.tableErrors = append(f.tableErrors, strings.TrimSpace(strings.TrimPrefix(trimmed, "==TABLEERROR ")))
		case trimmed == "==NOREADLINK":
			f.noReadlink = true
		case trimmed == "==END":
			f.complete = true
		case strings.HasPrefix(trimmed, "==PROC "):
			if pid, rest, ok := splitPIDLine(trimmed, "==PROC "); ok {
				f.comm[pid] = rest
			}
		case strings.HasPrefix(trimmed, "==ARGV0 "):
			if pid, rest, ok := splitPIDLine(trimmed, "==ARGV0 "); ok {
				f.argv0[pid] = rest
			}
		case strings.HasPrefix(trimmed, "==FD "):
			if pid, rest, ok := splitPIDLine(trimmed, "==FD "); ok {
				if inode, ok := socketInode(rest); ok {
					f.holders[inode] = append(f.holders[inode], pid)
				}
			}
		}
	}
	// An unterminated table is a truncated read. Dropping it keeps the pair of
	// "no ==END" and "no table" pointing at the same cause.
	return f
}

// splitPIDLine splits "<marker> <pid> <rest>", where rest runs to the end of
// the line because a process name may contain spaces.
func splitPIDLine(line, marker string) (int, string, bool) {
	rest := strings.TrimPrefix(line, marker)
	num, tail, ok := strings.Cut(strings.TrimSpace(rest), " ")
	if !ok {
		return 0, "", false
	}
	pid, err := strconv.Atoi(num)
	if err != nil {
		return 0, "", false
	}
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return 0, "", false
	}
	return pid, tail, true
}

// socketInode extracts the inode from a "socket:[12345]" symlink target.
func socketInode(link string) (string, bool) {
	rest, ok := strings.CutPrefix(link, "socket:[")
	if !ok {
		return "", false
	}
	inode, ok := strings.CutSuffix(rest, "]")
	if !ok || inode == "" {
		return "", false
	}
	return inode, true
}

// listeningInodes returns the inodes of the sockets listening on port, read
// from a server pod's own socket tables.
//
// Both families are considered. A server bound to IPv6 serves IPv4 clients and
// appears in tcp6 alone, which is F-019 in docs/findings.md and is exactly what
// the reference deployment does on 2049.
func listeningInodes(f listenerFacts, port uint16) ([]string, error) {
	if !f.complete {
		return nil, fmt.Errorf("the socket reader did not run to completion, so a listening socket would "+
			"read here as absent%s", tableErrSuffix(f))
	}
	var inodes []string
	var listening []uint16
	for path, body := range f.tables {
		conns, err := ParseProcNetTCP(body)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		for _, c := range conns {
			if c.State != tcpListen {
				continue
			}
			listening = append(listening, c.Local.Port())
			if c.Local.Port() == port && c.Inode != "" {
				inodes = append(inodes, c.Inode)
			}
		}
	}
	if len(inodes) == 0 {
		return nil, fmt.Errorf("nothing is listening on port %d here (listening on %s)%s",
			port, portList(listening), tableErrSuffix(f))
	}
	return inodes, nil
}

// pickHolder names the process holding one of inodes, from a scan of a node.
//
// It refuses rather than guesses wherever the join is not unambiguous, because
// the name it returns is about to be SIGKILLed by pattern across that node.
func pickHolder(f listenerFacts, inodes []string) (ServerProcess, error) {
	if !f.complete {
		return ServerProcess{}, fmt.Errorf("the process scan did not run to completion, so the holder of "+
			"the socket would read here as absent%s", tableErrSuffix(f))
	}
	pids := map[int]bool{}
	for _, inode := range inodes {
		for _, pid := range f.holders[inode] {
			pids[pid] = true
		}
	}
	if len(pids) == 0 {
		if f.noReadlink {
			return ServerProcess{}, fmt.Errorf("has no readlink, so which process holds the socket cannot " +
				"be read there")
		}
		return ServerProcess{}, fmt.Errorf("no process there holds it, which is what a socket held from " +
			"another network namespace on another node looks like")
	}

	// Distinct names rather than distinct pids: a server that preforks answers
	// on one socket from several processes, and killing all of them by name is
	// exactly the fault the case means to inject. Two different names holding
	// one socket is not that, and is refused.
	byName := map[string][]int{}
	for pid := range pids {
		byName[processName(f, pid)] = append(byName[processName(f, pid)], pid)
	}
	if len(byName) != 1 {
		return ServerProcess{}, fmt.Errorf("it is held by more than one kind of process (%s), so naming one "+
			"to signal would be a guess", strings.Join(sortedProcessNames(byName), ", "))
	}
	name := sortedProcessNames(byName)[0]
	if name == "" {
		return ServerProcess{}, fmt.Errorf("the process holding it has no name the kernel will report")
	}
	sort.Ints(byName[name])
	return ServerProcess{
		Name:  name,
		PID:   byName[name][0],
		Inode: strings.Join(inodes, ","),
	}, nil
}

// processName is what to match on the node for a pid.
//
// /proc/<pid>/comm is truncated to 15 characters by the kernel, and a name cut
// in half is both harder to read in a fault record and a broader match than it
// looks. Where argv[0] extends it, argv[0]'s base name is used instead; where
// the two disagree about anything but length, comm wins, because argv[0] is
// writable by the process and comm is not.
func processName(f listenerFacts, pid int) string {
	comm := strings.TrimSpace(f.comm[pid])
	base := path.Base(strings.TrimSpace(f.argv0[pid]))
	if comm == "" {
		if base == "." || base == "/" {
			return ""
		}
		return base
	}
	if len(comm) >= 15 && strings.HasPrefix(base, comm) {
		return base
	}
	return comm
}

func tableErrSuffix(f listenerFacts) string {
	if len(f.tableErrors) == 0 {
		return ""
	}
	return ". Unreadable socket tables: " + strings.Join(f.tableErrors, "; ")
}

func portList(ports []uint16) string {
	if len(ports) == 0 {
		return "nothing"
	}
	seen := map[uint16]bool{}
	var out []string
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	for _, p := range ports {
		if !seen[p] {
			seen[p] = true
			out = append(out, strconv.Itoa(int(p)))
		}
	}
	return strings.Join(out, ", ")
}

func sortedProcessNames(m map[string][]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
