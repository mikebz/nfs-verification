package framework

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// What an NFS server can attribute a connection to is the address it came
// from. Nothing in the Kubernetes API states it, and no export configuration
// format is portable, so the suite reads it where it is a fact: the server's
// own socket table. That is the wire, not the server's configuration or its
// logs, so nothing here assumes a particular NFS implementation.
//
// A client cannot see this for itself. Its mount records the address it
// presented, and an address rewritten in flight leaves that record untouched,
// which is why a case reads both and compares them rather than trusting either
// alone. The reason the answer matters is that the mount is made by the node's
// kernel and not by the pod: a per-client rule can therefore name a node, and
// every pod on that node inherits it. It cannot name a workload, because the
// server never sees one.

// Conn is one row of a server's socket table: who is connected to it, and in
// what state.
type Conn struct {
	// Local is the address the server is listening or answering on.
	Local netip.AddrPort
	// Peer is the address the server attributes the other end to. This is the
	// client identity an export rule would be matched against.
	Peer netip.AddrPort
	// State is the TCP state, decoded from the kernel's hex code.
	State string
	// Mapped records that the kernel reported this as an IPv4-mapped IPv6
	// address. Kept rather than normalised away: a server listening on IPv6
	// sees every IPv4 client in this form, and whether a rule written as a
	// plain IPv4 address matches it is a question a case has to be able to ask.
	Mapped bool
	// Inode is the socket's inode as the kernel wrote it, the only thing that
	// ties a row here to the process holding it in /proc/<pid>/fd. Empty when
	// the row was too short to carry one.
	Inode string
}

// String renders a connection the way a failure message wants it.
func (c Conn) String() string {
	s := fmt.Sprintf("%s <- %s [%s]", c.Local, c.Peer, c.State)
	if c.Mapped {
		s += " (IPv4-mapped)"
	}
	return s
}

// TCP states as the kernel writes them in /proc/net/tcp. Only the two the cases
// distinguish are named; the rest are rendered as their code so that an
// unexpected state is visible rather than silently read as something else.
const (
	tcpEstablished = "ESTABLISHED"
	tcpListen      = "LISTEN"
)

var tcpStates = map[string]string{
	"01": tcpEstablished,
	"02": "SYN_SENT",
	"03": "SYN_RECV",
	"04": "FIN_WAIT1",
	"05": "FIN_WAIT2",
	"06": "TIME_WAIT",
	"07": "CLOSE",
	"08": "CLOSE_WAIT",
	"09": "LAST_ACK",
	"0A": tcpListen,
	"0B": "CLOSING",
}

// ServerConns reads the socket table of a server pod.
//
// It runs `cat` on the two files rather than `ss` or `netstat`: a server image
// is not the tools image and carries whatever its vendor put in it, while
// /proc/net is the kernel's and is there in every container. The Fedora-based
// image this project runs against has neither ss nor netstat, which is how that
// was settled rather than assumed.
//
// Both files are read, because the address family a server listens on is its
// choice and not the cluster's: a server bound to IPv6 accepts IPv4 clients and
// reports them mapped, and reading only /proc/net/tcp on such a server finds no
// NFS connections at all and would report a healthy deployment as one nobody is
// talking to.
func ServerConns(ctx context.Context, c *Client, pod *corev1.Pod) ([]Conn, error) {
	container := ""
	if len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}
	// Missing files are tolerated, an unreadable one is not: a kernel without
	// IPv6 has no tcp6 file, and that is not the same as a container the suite
	// cannot look inside. The script reports the two families separately
	// because one exit status cannot say which of them it belongs to.
	script, err := RunScript("socket-table.sh", "peers-"+strings.ToLower(Cfg().RunID))
	if err != nil {
		return nil, err
	}
	r := c.Sh(ctx, pod.Namespace, pod.Name, container, script)
	if r.Err != nil {
		return nil, Blockedf("reading the socket table of server pod %s/%s: %v: %s. "+
			"This needs exec into the server's namespace; a kubeconfig without it, or an image with no "+
			"cat, cannot answer who the server thinks its clients are",
			pod.Namespace, pod.Name, r.Err, strings.TrimSpace(r.Combined()))
	}
	return parseSocketTables(r.Stdout, pod.Namespace, pod.Name)
}

// parseSocketTables reads what scripts/socket-table.sh prints: a status line per
// address family, and the family's table when it could be read.
//
// A family whose file does not exist contributes nothing and is not an error. A
// family that exists and could not be read is an error, because the connections
// it would have held are the ones a caller is about to conclude are absent.
func parseSocketTables(out, namespace, pod string) ([]Conn, error) {
	var conns []Conn
	var family, body string
	var seen int

	flush := func() error {
		if family == "" {
			return nil
		}
		status, table, _ := strings.Cut(body, "\n")
		status = strings.TrimSpace(status)
		switch {
		case status == "absent":
			return nil
		case strings.HasPrefix(status, "error"):
			return fmt.Errorf("the socket table %s of %s/%s exists and could not be read: %s. "+
				"The connections it holds would otherwise be reported as absent",
				family, namespace, pod, strings.TrimSpace(strings.TrimPrefix(status, "error")))
		case status != "ok":
			return fmt.Errorf("the socket table reader gave no status for %s of %s/%s, so it cannot be "+
				"told apart from a read that never happened: %q", family, namespace, pod, truncate(out, 200))
		}
		parsed, err := ParseProcNetTCP(table)
		if err != nil {
			return err
		}
		conns = append(conns, parsed...)
		seen++
		return nil
	}

	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "==FAMILY "); ok {
			if err := flush(); err != nil {
				return nil, err
			}
			family, body = strings.TrimSpace(rest), ""
			continue
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "==STATUS "); ok {
			body = strings.TrimSpace(rest) + "\n"
			continue
		}
		if family != "" {
			body += line + "\n"
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if seen == 0 {
		// Not a pedantic check. F-011 is an exec that reported success with an
		// empty stdout, and every caller here reads an empty result as "the
		// server has no clients", which is a claim about the deployment.
		return nil, fmt.Errorf("no socket table of %s/%s could be read, so a caller would take the "+
			"server to have no connections at all: %q", namespace, pod, truncate(out, 200))
	}
	return conns, nil
}

// ParseProcNetTCP parses the contents of /proc/net/tcp or /proc/net/tcp6.
//
// The format is fixed-width hex, and the addresses are written as 32-bit words
// in host byte order, which on every architecture this suite runs on is little
// endian. Getting that backwards does not produce an error, it produces
// plausible addresses belonging to nobody, which is the failure mode this
// parser has a unit test for.
func ParseProcNetTCP(contents string) ([]Conn, error) {
	var out []Conn
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		// The header names its first column "sl"; every data row starts with an
		// index. Short lines are the trailing newline and the separator.
		if len(fields) < 4 || fields[0] == "sl" {
			continue
		}
		local, err := parseProcAddr(fields[1])
		if err != nil {
			return nil, fmt.Errorf("socket table line %q: %w", line, err)
		}
		peer, err := parseProcAddr(fields[2])
		if err != nil {
			return nil, fmt.Errorf("socket table line %q: %w", line, err)
		}
		state, ok := tcpStates[strings.ToUpper(fields[3])]
		if !ok {
			state = "state-" + fields[3]
		}
		// The inode is the tenth column. Rows are only ever shorter than that
		// in a fixture, so a missing one is left empty rather than refused:
		// every caller that needs it says so, and the callers that read
		// addresses and states do not.
		inode := ""
		if len(fields) >= 10 {
			inode = fields[9]
		}
		out = append(out, Conn{
			Local: local, Peer: peer, State: state,
			Mapped: local.Addr().Is4In6() || peer.Addr().Is4In6(),
			Inode:  inode,
		})
	}
	return out, nil
}

// parseProcAddr decodes one "address:port" column.
func parseProcAddr(field string) (netip.AddrPort, error) {
	addrHex, portHex, ok := strings.Cut(field, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("address column %q has no port", field)
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("port %q: %w", portHex, err)
	}
	raw, err := hex.DecodeString(addrHex)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("address %q: %w", addrHex, err)
	}
	// Each 32-bit word is written in host byte order, so every four bytes are
	// reversed. A v4 address is one word; a v6 address is four.
	if len(raw)%4 != 0 || len(raw) == 0 {
		return netip.AddrPort{}, fmt.Errorf("address %q is %d bytes, want a multiple of 4", addrHex, len(raw))
	}
	be := make([]byte, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.BigEndian.PutUint32(be[i:i+4], binary.LittleEndian.Uint32(raw[i:i+4]))
	}
	addr, ok := netip.AddrFromSlice(be)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("address %q is not 4 or 16 bytes", addrHex)
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}

// PeersOn returns the distinct peer addresses connected to a port, established
// only, with the port and the mapped form dropped so that two connections from
// one client count once.
//
// Established only, because a socket in TIME_WAIT belongs to a client that has
// gone, and a case comparing "which clients does the server see" against pods
// that exist now would count a previous case's pod as a current client.
func PeersOn(conns []Conn, port uint16) []netip.Addr {
	seen := map[netip.Addr]bool{}
	for _, c := range conns {
		if c.State != tcpEstablished || c.Local.Port() != port {
			continue
		}
		seen[c.Peer.Addr().Unmap()] = true
	}
	out := make([]netip.Addr, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

// ListeningPorts returns the ports a server is listening on, sorted.
//
// SEC-08 reads it to say whether the deployment offers a transport-secured port
// at all, which is a statement about the server rather than about the mount the
// client happened to make.
func ListeningPorts(conns []Conn) []uint16 {
	seen := map[uint16]bool{}
	for _, c := range conns {
		if c.State == tcpListen {
			seen[c.Local.Port()] = true
		}
	}
	out := make([]uint16, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// NodeAddresses returns a node's internal addresses, which is what a server
// sees as the client: the mount is made by the node's kernel, in the host
// network namespace, so a pod's own address never reaches the server.
func NodeAddresses(ctx context.Context, c *Client, node string) ([]netip.Addr, error) {
	n, err := c.Kube.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range n.Status.Addresses {
		if a.Type != corev1.NodeInternalIP {
			continue
		}
		if addr, err := netip.ParseAddr(a.Address); err == nil {
			out = append(out, addr.Unmap())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("node %s reports no internal address", node)
	}
	return out, nil
}

// HostAddr resolves the server end of an NFS source to an address.
//
// The source comes out of the PV, so it is whatever the provisioner wrote
// there: on this architecture that is usually the Service's cluster IP, which
// is the proxy SEC-04 is about. A name that is not an address is returned
// unresolved rather than looked up, because the workstation's resolver is not
// the cluster's and an answer from it would be about the wrong network.
func HostAddr(src NFSSource) (netip.Addr, bool) {
	host := src.Server
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// truncate keeps a diagnostic quotable when the thing that failed produced a
// page of output.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
