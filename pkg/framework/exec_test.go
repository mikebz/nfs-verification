package framework

import (
	"os/exec"
	"testing"
)

// Shell strings built for exec inside pods must be safe against injection,
// spaces, and quoting slips. A quoting bug here fails inside a cluster during
// test execution.

// TestQuoteEscapesShellStrings verifies that Quote protects arbitrary strings
// when evaluated by a real shell.
//
// Steps:
//  1. Test various strings with special characters (spaces, single quotes, double quotes,
//     dollar signs, semicolons, backticks, newlines).
//  2. Run 'printf %s <quoted>' in /bin/sh.
//  3. Assert the shell output matches the input verbatim.
func TestQuoteEscapesShellStrings(t *testing.T) {
	sh := lookOrSkip(t, "sh")

	inputs := []string{
		"simple",
		"with spaces",
		"with'single'quotes",
		"with\"double\"quotes",
		"with$VAR_and_`command`",
		"with; semicolon && and || or",
		"new\nline",
		"",
		"   leading and trailing spaces   ",
		`path/with/\escapes/and/brackets/[0-9]/*`,
	}

	for _, input := range inputs {
		quoted := Quote(input)
		cmd := exec.Command(sh, "-c", "printf '%s' "+quoted)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("running shell with quoted %q (%s): %v", input, quoted, err)
		}
		if string(out) != input {
			t.Errorf("Quote(%q) = %s, shell produced %q", input, quoted, string(out))
		}
	}
}

// TestExecResultCombined verifies that Combined joins stdout and stderr
// cleanly and trims surrounding whitespace.
//
// Steps:
//  1. Test combinations of stdout and stderr.
//  2. Assert Combined formats them as expected.
func TestExecResultCombined(t *testing.T) {
	cases := []struct {
		name   string
		result ExecResult
		want   string
	}{
		{
			name:   "both stdout and stderr with existing trailing newlines",
			result: ExecResult{Stdout: "hello\n", Stderr: "world\n"},
			want:   "hello\n\nworld",
		},
		{
			name:   "both stdout and stderr without trailing newlines",
			result: ExecResult{Stdout: "hello", Stderr: "world"},
			want:   "hello\nworld",
		},
		{
			name:   "stdout only",
			result: ExecResult{Stdout: "output text  "},
			want:   "output text",
		},
		{
			name:   "stderr only",
			result: ExecResult{Stderr: "error text\n\n"},
			want:   "error text",
		},
		{
			name:   "both empty",
			result: ExecResult{},
			want:   "",
		},
	}

	for _, tc := range cases {
		if got := tc.result.Combined(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
