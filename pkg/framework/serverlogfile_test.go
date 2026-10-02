package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Reading the server's own log file is what lets grace be observed at all on
// both reference deployments. Its failures have no symptom: a file read with the
// wrong time zone, or cut off before the fault, reads as a server that never
// announced grace, and that is filed against the server.

// ganeshaLog is a failover as /export/ganesha.log holds it, Ganesha's date in
// front of each line. The last line is the newest, at 20:45:11 UTC.
func ganeshaLog() string {
	return "01/10/2026 20:30:00" + ganeshaExit + "\n" +
		"01/10/2026 20:43:40" + ganeshaEnter + "\n" +
		"01/10/2026 20:43:40" + ganeshaReload + "\n" +
		"01/10/2026 20:43:50" + ganeshaCheck + "\n" +
		"01/10/2026 20:45:11" + ganeshaExit + "\n"
}

// ganeshaLogNewest is the newest line's time, which a healthy file's mtime
// matches.
var ganeshaLogNewest = time.Date(2026, 10, 1, 20, 45, 11, 0, time.UTC)

// TestServerLogScriptReadsTheFilesTheProcessNames runs the shipped reader under
// a real shell against a fixture proc tree holding the reference server's
// command line, so a quoting slip fails here rather than inside OBS-03.
//
// Steps:
//  1. Build a proc tree for one process whose command line is ganesha.nfsd's
//     on both reference deployments, with its log and configuration under the
//     process's root and its pid file missing.
//  2. Run the script and parse what it printed.
//  3. Require the log and the configuration to come back, and the missing
//     file not to.
//  4. Read the files as logs from just before the failover, and require the
//     log to give one entry and one exit, and the configuration, which sets
//     Grace_Period as the reference server's does, to be no log and no gap.
//  5. Run it for a pid with no command line, and for one that is not a
//     number, and require an error from each rather than an empty read.
func TestServerLogScriptReadsTheFilesTheProcessNames(t *testing.T) {
	restore := *Cfg()
	t.Cleanup(func() { *Cfg() = restore })
	Cfg().GraceEnterPattern, Cfg().GraceExitPattern = "", ""
	if err := compileGracePatterns(); err != nil {
		t.Fatalf("resetting the wording flags: %v", err)
	}

	root := filepath.Join(t.TempDir(), "proc")
	proc := filepath.Join(root, "940409")
	export := filepath.Join(proc, "root", "export")
	if err := os.MkdirAll(export, 0o755); err != nil {
		t.Fatalf("creating %s: %v", export, err)
	}
	cmdline := strings.Join([]string{"ganesha.nfsd", "-F", "-L", "/export/ganesha.log",
		"-p", "/var/run/ganesha.pid", "-f", "/export/vfs.conf"}, "\x00") + "\x00"
	files := map[string]string{
		filepath.Join(proc, "cmdline"):       cmdline,
		filepath.Join(export, "ganesha.log"): ganeshaLog(),
		filepath.Join(export, "vfs.conf"):    "EXPORT { Path = /export; Pseudo = /; }\nNFSv4 {\n\tGrace_Period = 90;\n}\n",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	if err := os.Chtimes(filepath.Join(export, "ganesha.log"), ganeshaLogNewest, ganeshaLogNewest); err != nil {
		t.Fatalf("setting the log's mtime: %v", err)
	}
	script := materializeScript(t, "server-log.sh")

	out, err := exec.Command("sh", script, root, "940409", "1048576").CombinedOutput()
	if err != nil {
		t.Fatalf("running the reader: %v: %s", err, out)
	}
	read := parseServerLogRead(string(out))
	if !read.complete || len(read.errors) != 0 {
		t.Fatalf("the reader reported complete=%v errors=%v\n%s", read.complete, read.errors, out)
	}
	byPath := map[string]serverLogFile{}
	for _, f := range read.files {
		byPath[f.path] = f
	}
	if len(byPath) != 2 || byPath["/export/ganesha.log"].path == "" || byPath["/export/vfs.conf"].path == "" {
		t.Fatalf("read %v, want the log and the configuration and not the missing pid file\n%s", read.files, out)
	}

	log := byPath["/export/ganesha.log"]
	if !log.mtime.Equal(ganeshaLogNewest) {
		t.Errorf("mtime read as %s, want %s", log.mtime.UTC(), ganeshaLogNewest)
	}
	cut := time.Date(2026, 10, 1, 20, 43, 0, 0, time.UTC)
	lines, isLog, gap := log.lines("server-0:/export/ganesha.log", cut)
	if !isLog || gap != "" {
		t.Fatalf("the log read as isLog=%v gap=%q", isLog, gap)
	}
	obs := classifyGraceLines(lines)
	w, ok := obs.Window()
	if len(obs.Entries()) != 1 || !ok || w.Duration() != 91*time.Second {
		t.Errorf("the log gave %s, want one entry and a 91s window; the exit from 20:30, before the cut, "+
			"must not count", obs.Describe())
	}
	if _, isLog, gap := byPath["/export/vfs.conf"].lines("server-0:/export/vfs.conf", cut); isLog || gap != "" {
		t.Errorf("the configuration read as isLog=%v gap=%q, want no log and no gap", isLog, gap)
	}

	for _, pid := range []string{"99999", "1;true"} {
		out, err := exec.Command("sh", script, root, pid, "1048576").CombinedOutput()
		if err != nil {
			t.Fatalf("running the reader for %q: %v: %s", pid, err, out)
		}
		if r := parseServerLogRead(string(out)); len(r.errors) == 0 {
			t.Errorf("pid %q read as %+v rather than an error\n%s", pid, r, out)
		}
	}
}

// TestServerLogFileRefusesWhatItCannotPlace covers the files a timestamp check
// exists for. Each would otherwise put grace at the wrong moment or nowhere,
// and nothing would say so.
//
// Steps:
//  1. Read a file whose mtime is an hour from its newest line, which is what
//     a local time zone looks like, and require a gap and no lines.
//  2. Read a file that was cut off and begins after the window opened, and
//     require its lines with a gap that says the start was not seen; require
//     the partial first line to be dropped.
//  3. Read a file that announces grace with no timestamp this suite reads,
//     and require a gap rather than a silent non-log.
//  4. Require a reader's output with no end marker to read as incomplete, and
//     a file with no end marker to be dropped.
func TestServerLogFileRefusesWhatItCannotPlace(t *testing.T) {
	cut := time.Date(2026, 10, 1, 20, 43, 0, 0, time.UTC)

	shifted := serverLogFile{path: "/x.log", size: 100, mtime: ganeshaLogNewest.Add(time.Hour), body: ganeshaLog()}
	if lines, _, gap := shifted.lines("x", cut); len(lines) != 0 || !strings.Contains(gap, "not being read") {
		t.Errorf("a file an hour from its mtime gave %d lines and gap %q, want none and a gap", len(lines), gap)
	}

	partial := "XX NOT IN GRACE\n" + "01/10/2026 20:45:11" + ganeshaExit + "\n"
	cutOff := serverLogFile{path: "/x.log", size: serverLogTailBytes + 1, mtime: ganeshaLogNewest, body: partial}
	lines, isLog, gap := cutOff.lines("x", cut)
	if !isLog || len(lines) != 1 || !strings.Contains(gap, "only its last") {
		t.Errorf("a cut-off file gave isLog=%v, %d lines and gap %q, want the one whole line and a gap "+
			"saying the start was not seen", isLog, len(lines), gap)
	}

	undated := serverLogFile{path: "/x.log", size: 20, mtime: ganeshaLogNewest, body: "[main] NFS Server Now IN GRACE\n"}
	if _, isLog, gap := undated.lines("x", cut); isLog || !strings.Contains(gap, "announces grace") {
		t.Errorf("a file announcing grace with no timestamp gave isLog=%v gap %q, want a gap", isLog, gap)
	}

	short := parseServerLogRead("==FILE 10 1790887511 /x.log\n01/10/2026 20:45:11 a\n")
	if short.complete || len(short.files) != 0 {
		t.Errorf("output with no end markers read as complete=%v with %d files", short.complete, len(short.files))
	}
}
