// Command locktool takes and reports fcntl(2) byte-range locks from inside a
// test pod.
//
// It exists because nothing on a stock image can do this. flock(1) calls
// flock(2), whose system call has no range argument; util-linux ships no
// byte-range lock command at all; and the alternatives that would work all need
// a runtime the repository does not assume. Section 5.2 of
// docs/05-data-path-and-locktool-design.md lists what was checked.
//
// Byte ranges are not an exotic thing to reach for here: NFSv4.1 carries LOCK,
// LOCKT and LOCKU with an offset and a length in the protocol itself (RFC 8881),
// and the Linux client already sends one for every lock an application takes,
// flock included. Whole-file is the degenerate case of a range, not a different
// mechanism. What the suite could not express before this binary is two clients
// holding *different* parts of one file.
//
// Usage:
//
//	locktool hold  <path> <start> <len> <mode> <run-file> <state-file>
//	locktool try   <path> <start> <len> <mode>
//	locktool getlk <path> <start> <len> <mode>
//
//	path        file to lock; created if absent, as the shell lock probe does
//	start       l_start, absolute, from the start of the file
//	len         l_len; 0 means "to end of file", which is how a whole-file
//	            fcntl lock is expressed
//	mode        read or write, for F_RDLCK or F_WRLCK
//	run-file    hold releases once this is removed
//	state-file  hold reports waiting, held, released or failed here
//
// The run and state files belong on the pod's own filesystem, never on the
// share: a holder that reported its own progress through the filesystem under
// test could not tell a stalled harness from stalled storage.
//
// Output is one line, every field a fixed token or an integer:
//
//	hold   launched
//	try    GRANTED | REFUSED type=<r|w> start=<n> len=<n> | REFUSED
//	getlk  FREE | HELD type=<r|w> start=<n> len=<n>
//
// Nothing prints a pid. The protocol does not carry one: a denied LOCK or LOCKT
// gives the conflicting offset, length and type plus an opaque lock owner, and
// nothing that identifies a process on another node. Whatever l_pid holds after
// a cross-client F_GETLK cannot name a remote process, so printing it would
// invite a reader to trust a number that means nothing.
//
// Exit codes: 0 granted or free, 1 refused or held, 2 usage or I/O error.
// Refused and error are separated because a refusal is the expected result in
// half the cases that use this, and an error never is.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

const (
	// exitOK covers granted and free.
	exitOK = 0
	// exitRefused covers refused and held: an expected answer, not a fault.
	exitRefused = 1
	// exitError covers a usage mistake or an I/O failure.
	exitError = 2
)

// workerEnv marks the detached second pass of hold. The first pass returns to
// the caller immediately, exactly as the shell holders in pkg/framework/scripts
// do, so that an exec which starts a holder is not blocked for its lifetime.
const workerEnv = "NFSV_LOCKTOOL_WORKER"

const (
	// acquireInterval is how often a refused hold retries. One attempt per
	// second, matching the shell lock probe: the windows these feed are tens of
	// seconds and a tighter loop would sharpen no assertion.
	acquireInterval = time.Second
	// releaseInterval is how often a holder checks whether it has been told to
	// let go. Shorter than the acquire rate because a case that releases a lock
	// then waits for another client to take it pays this delay twice.
	releaseInterval = 200 * time.Millisecond
)

func main() {
	if len(os.Args) < 2 {
		usage("no subcommand")
	}
	switch os.Args[1] {
	case "hold":
		hold(os.Args[2:])
	case "try":
		try(os.Args[2:])
	case "getlk":
		getlk(os.Args[2:])
	default:
		usage("unknown subcommand " + strconv.Quote(os.Args[1]))
	}
}

func usage(problem string) {
	fmt.Fprintf(os.Stderr, "locktool: %s\n\n"+
		"usage:\n"+
		"  locktool hold  <path> <start> <len> <mode> <run-file> <state-file>\n"+
		"  locktool try   <path> <start> <len> <mode>\n"+
		"  locktool getlk <path> <start> <len> <mode>\n\n"+
		"mode is read or write; len 0 means to end of file\n", problem)
	os.Exit(exitError)
}

// lockArgs are the four values every subcommand takes.
type lockArgs struct {
	path   string
	start  int64
	length int64
	mode   string
}

// parseLockArgs reads the four common arguments. Everything is validated here
// rather than at the system call, so a mistake in a case names the argument.
func parseLockArgs(argv []string) (lockArgs, error) {
	if len(argv) < 4 {
		return lockArgs{}, errors.New("want <path> <start> <len> <mode>")
	}
	a := lockArgs{path: argv[0], mode: argv[3]}
	var err error
	if a.start, err = strconv.ParseInt(argv[1], 10, 64); err != nil || a.start < 0 {
		return lockArgs{}, fmt.Errorf("start %q is not a non-negative integer", argv[1])
	}
	if a.length, err = strconv.ParseInt(argv[2], 10, 64); err != nil || a.length < 0 {
		return lockArgs{}, fmt.Errorf("len %q is not a non-negative integer; 0 means to end of file", argv[2])
	}
	if _, err := lockType(a.mode); err != nil {
		return lockArgs{}, err
	}
	return a, nil
}

// lockType maps the mode token onto the fcntl lock type.
func lockType(mode string) (int16, error) {
	switch mode {
	case "read":
		return syscall.F_RDLCK, nil
	case "write":
		return syscall.F_WRLCK, nil
	}
	return 0, fmt.Errorf("mode %q is not read or write", mode)
}

// openTarget opens the file the lock applies to, creating it if absent, as the
// shell lock probe already does. O_RDWR because a write lock needs a writable
// descriptor and a read lock a readable one, and one mode serves both.
func openTarget(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}

// flockFor builds the lock description for a request.
func flockFor(a lockArgs, typ int16) syscall.Flock_t {
	// Whence is SEEK_SET: start is absolute, so a case naming a range names the
	// same bytes whatever the descriptor's offset happens to be.
	return syscall.Flock_t{Type: typ, Whence: 0, Start: a.start, Len: a.length}
}

// setLock attempts a non-blocking acquire and, when the kernel refuses, asks
// what stands in the way.
//
// F_SETLK, never F_SETLKW. A blocking acquire cannot notice its run-file being
// removed, so a holder that is refused sits in the kernel with nothing the
// harness can do about it, and on a hard mount the process may be unkillable in
// D state, which leaves the pod Terminating and turns teardown into the
// F-001-adjacent path it exists to avoid.
func setLock(f *os.File, a lockArgs, typ int16) (granted bool, conflict syscall.Flock_t, known bool, err error) {
	lk := flockFor(a, typ)
	err = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk)
	if err == nil {
		return true, syscall.Flock_t{}, false, nil
	}
	if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EAGAIN) {
		return false, syscall.Flock_t{}, false, err
	}
	// Refused. A second call asks who holds it, and may find the conflict has
	// already cleared: the refusal still happened, so it is reported either
	// way, with the range only when there is one to report.
	//
	// A query that itself fails is reported on stderr rather than swallowed or
	// turned into an error. The acquire really was refused, and promoting a
	// transient RPC failure on the follow-up query into an exit code 2 would
	// fail cases whose expected result is a refusal. But losing the fact
	// entirely leaves an unattributed refusal that looks exactly like a
	// conflict that cleared, which is a different thing.
	c, gerr := getLock(f, a, typ)
	if gerr != nil {
		fmt.Fprintf(os.Stderr, "locktool: the lock was refused and the follow-up query failed, so the "+
			"refusal cannot be attributed: %v\n", gerr)
		return false, syscall.Flock_t{}, false, nil
	}
	if c.Type == syscall.F_UNLCK {
		return false, syscall.Flock_t{}, false, nil
	}
	return false, c, true, nil
}

// getLock asks F_GETLK what would stand in the way of this request without
// taking anything.
//
// This is the difference between asking and acquiring. F_SETLK answers the same
// question by taking the lock, which changes the state every later attempt
// observes; a case asserting that a range is still held by its original holder
// cannot use a probe that acquires.
//
// A lock the calling process already holds is not reported as a conflict, which
// is why every query here comes from a different pod than the holder.
func getLock(f *os.File, a lockArgs, typ int16) (syscall.Flock_t, error) {
	lk := flockFor(a, typ)
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		return syscall.Flock_t{}, err
	}
	return lk, nil
}

// unlock drops a held range explicitly, before the descriptor closes, so that
// "released" in the state file cannot be read before the lock is really gone.
func unlock(f *os.File, a lockArgs) error {
	lk := flockFor(a, syscall.F_UNLCK)
	return syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk)
}

// describe renders a conflicting lock as the harness parses it.
func describe(lk syscall.Flock_t) string {
	t := "w"
	if lk.Type == syscall.F_RDLCK {
		t = "r"
	}
	return fmt.Sprintf("type=%s start=%d len=%d", t, lk.Start, lk.Len)
}

// try attempts the lock once and reports the answer. The lock, if granted, goes
// with the process: this subcommand is a question, not a holder.
func try(argv []string) {
	a, err := parseLockArgs(argv)
	if err != nil {
		usage(err.Error())
	}
	f, err := openTarget(a.path)
	if err != nil {
		fail("opening %s: %v", a.path, err)
	}
	defer f.Close()
	typ, _ := lockType(a.mode)

	granted, conflict, known, err := setLock(f, a, typ)
	if err != nil {
		fail("locking %s: %v", a.path, err)
	}
	if granted {
		fmt.Println("GRANTED")
		os.Exit(exitOK)
	}
	if known {
		fmt.Printf("REFUSED %s\n", describe(conflict))
	} else {
		// Refused, and the conflict had already cleared by the time we asked.
		fmt.Println("REFUSED")
	}
	os.Exit(exitRefused)
}

// getlk reports who holds a range without taking it.
func getlk(argv []string) {
	a, err := parseLockArgs(argv)
	if err != nil {
		usage(err.Error())
	}
	f, err := openTarget(a.path)
	if err != nil {
		fail("opening %s: %v", a.path, err)
	}
	defer f.Close()
	typ, _ := lockType(a.mode)

	lk, err := getLock(f, a, typ)
	if err != nil {
		fail("querying %s: %v", a.path, err)
	}
	if lk.Type == syscall.F_UNLCK {
		fmt.Println("FREE")
		os.Exit(exitOK)
	}
	fmt.Printf("HELD %s\n", describe(lk))
	os.Exit(exitRefused)
}

// hold takes a lock and keeps it until the run-file is removed. The first pass
// sets up and relaunches detached; the second pass is the holder.
func hold(argv []string) {
	if len(argv) < 6 {
		usage("hold wants <path> <start> <len> <mode> <run-file> <state-file>")
	}
	a, err := parseLockArgs(argv)
	if err != nil {
		usage(err.Error())
	}
	runFile, stateFile := argv[4], argv[5]

	if os.Getenv(workerEnv) == "" {
		// waiting, not empty: the caller polls this file, and a fourth state
		// beats inferring a blocked acquire from a file with nothing in it.
		if err := writeState(stateFile, "waiting"); err != nil {
			fail("writing the state file %s: %v", stateFile, err)
		}
		if err := os.WriteFile(runFile, nil, 0o644); err != nil {
			fail("writing the run file %s: %v", runFile, err)
		}
		if err := relaunch(); err != nil {
			fail("relaunching detached: %v", err)
		}
		fmt.Println("launched")
		os.Exit(exitOK)
	}
	worker(a, runFile, stateFile)
}

// relaunch starts the detached second pass. Setsid puts it in its own session,
// so it outlives the exec stream that started it, which is the same shape the
// shell holders use.
func relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), workerEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// worker is the detached holder.
func worker(a lockArgs, runFile, stateFile string) {
	// Opened once, and that one descriptor is kept. POSIX record locks are
	// dropped when *any* descriptor to the file is closed by the process, so a
	// retry loop that reopened would drop a lock it had already been granted.
	// That rule is the reason OFD locks exist; see section 5.1 of the design.
	f, err := openTarget(a.path)
	if err != nil {
		_ = writeState(stateFile, "failed")
		fail("opening %s: %v", a.path, err)
	}
	defer f.Close()
	typ, _ := lockType(a.mode)

	for exists(runFile) {
		granted, _, _, err := setLock(f, a, typ)
		if err != nil {
			_ = writeState(stateFile, "failed")
			fail("locking %s: %v", a.path, err)
		}
		if granted {
			if err := writeState(stateFile, "held"); err != nil {
				fail("writing the state file %s: %v", stateFile, err)
			}
			for exists(runFile) {
				time.Sleep(releaseInterval)
			}
			if err := unlock(f, a); err != nil {
				_ = writeState(stateFile, "failed")
				fail("unlocking %s: %v", a.path, err)
			}
			_ = writeState(stateFile, "released")
			os.Exit(exitOK)
		}
		time.Sleep(acquireInterval)
	}
	// The run file went while the acquire was still refused. The holder is
	// stopping and holds nothing, which is what released means to a caller;
	// the state file said "waiting" for the whole of its life, which is where a
	// case reads that it never got the lock.
	_ = writeState(stateFile, "released")
	os.Exit(exitOK)
}

func writeState(path, state string) error {
	return os.WriteFile(path, []byte(state+"\n"), 0o644)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "locktool: "+format+"\n", args...)
	os.Exit(exitError)
}
