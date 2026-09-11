package framework

import "testing"

// locktool's output is one line, and the assertions in DATA-05, DATA-06 and
// CHAOS-06 are read from it. A parser that silently mis-reads a refusal as a
// grant reports a protocol violation on a server that behaved correctly, so
// this is checked against the tool's own shapes rather than against a cluster.

// TestParseLockAnswer covers every line locktool can print, and the shapes it
// must reject.
//
// Steps:
//  1. Parse each of GRANTED, FREE, REFUSED with a conflict, bare REFUSED, and
//     HELD.
//  2. Check the range and type came back as printed.
//  3. Assert malformed lines are errors rather than a default answer, since a
//     default here would be "free" and would grant on a mis-read.
func TestParseLockAnswer(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		wantFree bool
		wantErr  bool
		conflict LockConflict
	}{
		{name: "granted", out: "GRANTED\n", wantFree: true},
		{name: "free", out: "FREE\n", wantFree: true},
		{
			name: "refused-with-range", out: "REFUSED type=w start=4096 len=8192\n",
			conflict: LockConflict{Known: true, Mode: "write", Start: 4096, Len: 8192},
		},
		{
			name: "held-read-to-eof", out: "HELD type=r start=0 len=0\n",
			conflict: LockConflict{Known: true, Mode: "read", Start: 0, Len: 0},
		},
		{
			// The conflict cleared between the acquire and the query. The
			// refusal still happened and is still reported.
			name: "refused-unattributed", out: "REFUSED\n",
		},
		{name: "empty", out: "", wantErr: true},
		{name: "unknown-verb", out: "MAYBE type=w start=0 len=0", wantErr: true},
		{name: "granted-with-fields", out: "GRANTED type=w start=0 len=0", wantErr: true},
		{name: "bad-type", out: "HELD type=x start=0 len=0", wantErr: true},
		{name: "missing-field", out: "HELD type=w start=0", wantErr: true},
		{name: "non-numeric-start", out: "HELD type=w start=beginning len=1", wantErr: true},
		{name: "unknown-field", out: "HELD type=w start=0 len=1 pid=7", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLockAnswer(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsed %q as %+v, want an error: an unreadable answer must never "+
						"default to free", tc.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.out, err)
			}
			if got.Free != tc.wantFree {
				t.Errorf("free is %v for %q, want %v", got.Free, tc.out, tc.wantFree)
			}
			if got.Conflict != tc.conflict {
				t.Errorf("conflict is %+v for %q, want %+v", got.Conflict, tc.out, tc.conflict)
			}
		})
	}
}

// TestParseLockAnswerRejectsAPid guards the one field the tool deliberately
// does not print. NFSv4.1 carries no pid: a denied LOCK or LOCKT gives the
// conflicting range, type and an opaque lock owner, and nothing that names a
// process on another node. A parser that accepted one would invite an assertion
// on a number that means nothing.
//
// Steps:
//  1. Parse a refusal carrying a pid field.
//  2. Assert it is rejected. A parser that accepted one would invite an
//     assertion on a number that cannot name a process on another node, and a
//     cross-node lock assertion built on it would be meaningless.
func TestParseLockAnswerRejectsAPid(t *testing.T) {
	if _, err := ParseLockAnswer("REFUSED type=w start=0 len=1 pid=4321"); err == nil {
		t.Error("a pid field was accepted; the protocol carries none, so no assertion may read one")
	}
}

// TestLockRangeArgs checks the arguments a range becomes on the tool's command
// line.
//
// Steps:
//  1. Render a range as the tool's positional arguments and compare them.
//  2. Assert a zero length renders as "to end of file" rather than as an empty
//     range. The two are opposite meanings, and a whole-file lock asked for as
//     an empty one would lock nothing and be granted.
func TestLockRangeArgs(t *testing.T) {
	got := WriteRange(4096, 0).args()
	want := []string{"4096", "0", "write"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if r := ReadRange(0, 0); r.String() != "read [0..EOF)" {
		t.Errorf("a zero length rendered as %q; zero means to end of file, not an empty range", r)
	}
}

// TestLockToolCmdQuotesItsArguments covers the paths the tool is handed.
//
// Steps:
//  1. Build an invocation over a path holding a space.
//  2. Assert every argument arrived quoted. Paths come from a case and are
//     interpolated into a shell command inside a pod, and a path with a space
//     or a quote in it is exactly what a careless helper splits in two.
func TestLockToolCmdQuotesItsArguments(t *testing.T) {
	got := lockToolCmd("try", "/mnt/share/a lock file", WriteRange(0, 16))
	want := `'/tmp/locktool' 'try' '/mnt/share/a lock file' '0' '16' 'write'`
	if got != want {
		t.Errorf("built %q, want %q", got, want)
	}
}
