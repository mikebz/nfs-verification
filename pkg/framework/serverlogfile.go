package framework

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mikebz/nfs-verification/pkg/slo"
)

// The server's log stream is not the only place a server says what it is doing,
// and on both reference deployments it is not where grace is announced at all:
// the container prints its supervisor's banner, and ganesha.nfsd writes grace
// to a file on the export volume. A suite that read only the stream reported
// those servers as silent about grace, which is F-008 and F-030 in
// docs/findings.md.
//
// So the observer also reads the files the serving process writes. Which files
// those are is discovered rather than declared: every absolute path the process
// names on its own command line is a candidate, and a candidate is a log when
// its lines carry timestamps this suite can read. That uses no implementation's
// option syntax, so a server that spells its log option differently is found
// the same way. It was going to be the files the process holds open for
// append, and F-030 says why it is not: Ganesha opens its log for each write
// and holds nothing between them.
//
// The read goes through the node agent, for the reason F-027 gives: from inside
// the pod, another process's root and descriptors are unreadable without
// CAP_SYS_PTRACE. It is taken live, when a case observes, because what is in
// the file is what the case's own fault just changed.

const (
	// serverLogTailBytes is how much of the end of each file is read. A
	// failover adds tens of lines to the reference server's log, so this holds
	// thousands of lines of margin; where it does not, the observation says
	// so rather than reading a cut-off file as one that never mentioned grace.
	serverLogTailBytes = 1 << 20

	// serverLogReadTimeout bounds one node read, so a node that stops
	// answering costs one gap rather than the whole observation.
	serverLogReadTimeout = 20 * time.Second

	// serverLogMtimeSlack is how far a file's newest timestamp may sit from
	// its modification time. Both come from the same node's clock, and a line
	// is stamped as it is written, so the two agree to the second; the slack
	// is for one-second resolution on both sides and a write that lands
	// between reading the one and the other. A file that disagrees by more is
	// one whose timestamps are not being read the way they were written.
	serverLogMtimeSlack = 5 * time.Second
)

// ganeshaStampLayout is the date and time NFS-Ganesha writes at the head of a
// log line by default: day first, then month, in the server's local time. It is
// this implementation's format, not a convention, which is why every file read
// with it is checked against its own modification time before a line of it is
// believed: a month-first date or a local time zone shows up there as a file
// whose newest line was written at a different moment from the file.
const ganeshaStampLayout = "02/01/2006 15:04:05"

// ServerLogFiles reads the log files the server pods' serving processes write,
// from since onwards. It returns the lines, the files that were read, and what
// could not be read. Nothing it cannot read is an error: the observation it
// feeds names each gap, so that a file the suite could not reach is not taken
// for a server that said nothing.
func ServerLogFiles(ctx context.Context, c *Client, agent *Agent, since time.Time) ([]LogLine, []string, []string) {
	pods, err := ServerPods(ctx, c)
	if err != nil {
		return nil, nil, []string{fmt.Sprintf("discovering the server pods to read their files: %v", err)}
	}
	cut := since
	if !cut.IsZero() {
		// The same relaxation as the log stream's: since is the workstation's
		// clock and the lines are the server node's.
		cut = cut.Add(-slo.ClockSkewGuard)
	}
	var lines []LogLine
	var sources, gaps []string
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			// The pod the fault just deleted. Its process is going or gone,
			// and its files are on the volume its replacement mounts.
			continue
		}
		var containers []string
		for _, ct := range p.Spec.Containers {
			containers = append(containers, ct.Name)
		}
		sp, err := DiscoverServerProcess(ctx, c, agent, p.Namespace, p.Name, p.Spec.NodeName, containers, NFSPort)
		if err != nil {
			gaps = append(gaps, fmt.Sprintf("%s: the process serving NFS could not be named, so the files it "+
				"writes were not read: %v", p.Name, err))
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, serverLogReadTimeout)
		out, err := agent.RunScript(readCtx, sp.Node, "server-log.sh", "serverlog-"+strings.ToLower(Cfg().RunID),
			"/proc", strconv.Itoa(sp.PID), strconv.Itoa(serverLogTailBytes))
		cancel()
		if err != nil {
			gaps = append(gaps, fmt.Sprintf("%s: reading the files %s names on %s: %v", p.Name, sp.Name, sp.Node, err))
			continue
		}
		read := parseServerLogRead(out)
		if !read.complete {
			// F-011: an exec that returns success with part of the output.
			gaps = append(gaps, fmt.Sprintf("%s: the file reader on %s did not run to completion", p.Name, sp.Node))
		}
		for _, e := range read.errors {
			gaps = append(gaps, fmt.Sprintf("%s: %s", p.Name, e))
		}
		for _, f := range read.files {
			src := p.Name + ":" + f.path
			ls, isLog, gap := f.lines(src, cut)
			if gap != "" {
				gaps = append(gaps, gap)
			}
			if isLog {
				sources = append(sources, src)
				lines = append(lines, ls...)
			}
		}
	}
	return lines, sources, gaps
}

// serverLogFile is one file as scripts/server-log.sh printed it.
type serverLogFile struct {
	path  string
	size  int64
	mtime time.Time
	body  string
}

// serverLogRead is everything the reader printed. It holds no conclusions.
type serverLogRead struct {
	files    []serverLogFile
	errors   []string
	complete bool
}

// parseServerLogRead reads the reader's output. A file with no end marker is a
// truncated read and is dropped, which leaves the missing ==END to say why.
func parseServerLogRead(out string) serverLogRead {
	var r serverLogRead
	var cur *serverLogFile
	var body []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case cur != nil && line == "==ENDFILE":
			cur.body = strings.Join(body, "\n")
			r.files = append(r.files, *cur)
			cur, body = nil, nil
		case cur != nil:
			body = append(body, line)
		case strings.HasPrefix(line, "==FILE "):
			f, err := parseFileHeader(strings.TrimPrefix(line, "==FILE "))
			if err != nil {
				r.errors = append(r.errors, err.Error())
				continue
			}
			cur = &f
		case strings.HasPrefix(line, "==ERROR "):
			r.errors = append(r.errors, strings.TrimPrefix(line, "==ERROR "))
		case line == "==END":
			r.complete = true
		}
	}
	return r
}

// parseFileHeader reads "<size> <mtime> <path>", where the path runs to the
// end of the line because a path may contain spaces.
func parseFileHeader(h string) (serverLogFile, error) {
	fields := strings.SplitN(h, " ", 3)
	if len(fields) != 3 {
		return serverLogFile{}, fmt.Errorf("unreadable file header %q", h)
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return serverLogFile{}, fmt.Errorf("file header %q: size: %w", h, err)
	}
	mtime, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return serverLogFile{}, fmt.Errorf("file header %q: mtime: %w", h, err)
	}
	return serverLogFile{path: fields[2], size: size, mtime: time.Unix(mtime, 0)}, nil
}

// lines returns the file's lines from cut onwards. isLog is false for a file
// in which no line carries a timestamp, which is what a configuration or pid
// file looks like. gap is set where the file could not be read as it was
// written, or was read only in part.
func (f serverLogFile) lines(source string, cut time.Time) (out []LogLine, isLog bool, gap string) {
	body := f.body
	truncated := f.size > serverLogTailBytes
	if truncated {
		// The read began partway through a line.
		if _, rest, ok := strings.Cut(body, "\n"); ok {
			body = rest
		}
	}
	var newest, oldest time.Time
	announces, keep := false, false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		at, rest, ok := splitServerStamp(line)
		if !ok {
			// Only a line that would be a transition if it carried a time
			// counts: the reference server's configuration says
			// "Grace_Period = 90;", and that is no announcement.
			if _, ok := classifyGrace(line); ok {
				announces = true
			}
			// A continuation of the line before it, kept or dropped with it.
			if keep {
				out = append(out, LogLine{Text: line, Source: source})
			}
			continue
		}
		if oldest.IsZero() {
			oldest = at
		}
		if at.After(newest) {
			newest = at
		}
		keep = cut.IsZero() || !at.Before(cut)
		if keep {
			out = append(out, LogLine{At: at, Text: rest, Source: source})
		}
	}
	if newest.IsZero() {
		if announces {
			return nil, false, fmt.Sprintf("%s announces grace, but no line in it starts with a timestamp this "+
				"suite reads (RFC 3339, or Ganesha's dd/mm/yyyy hh:mm:ss), so nothing in it can be placed "+
				"against the fault", source)
		}
		return nil, false, ""
	}
	if d := newest.Sub(f.mtime); d > serverLogMtimeSlack || d < -serverLogMtimeSlack {
		return nil, false, fmt.Sprintf("%s: its newest line reads as %s UTC, but the file was last written at "+
			"%s UTC, so its timestamps are not being read the way the server wrote them (a local time zone, or "+
			"a month-first date); none of its lines were used", source,
			newest.UTC().Format(time.RFC3339), f.mtime.UTC().Format(time.RFC3339))
	}
	if truncated && !cut.IsZero() && oldest.After(cut) {
		gap = fmt.Sprintf("%s: only its last %d bytes were read, and they begin at %s, after the window opened "+
			"at %s, so anything it said about grace before then was not seen", source, serverLogTailBytes,
			oldest.UTC().Format(time.RFC3339), cut.UTC().Format(time.RFC3339))
	}
	return out, true, gap
}

// splitServerStamp peels the timestamp a server wrote off the front of its own
// log line: RFC 3339 where the server writes that, and otherwise Ganesha's
// default, read as UTC and checked by the caller against the file's mtime.
func splitServerStamp(line string) (time.Time, string, bool) {
	if at, rest, ok := splitLogTimestamp(line); ok {
		return at, rest, true
	}
	n := len(ganeshaStampLayout)
	if len(line) < n {
		return time.Time{}, "", false
	}
	at, err := time.ParseInLocation(ganeshaStampLayout, line[:n], time.UTC)
	if err != nil {
		return time.Time{}, "", false
	}
	return at, line[n:], true
}
