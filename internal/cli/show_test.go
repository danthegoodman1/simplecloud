package cli

import "testing"

// exec must send exactly what was typed. Joining with spaces would let the remote
// shell re-parse quotes, so a one-liner with its own quoting would break.
func TestShellJoinPreservesArguments(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"ls", "-l"}, `'ls' '-l'`},
		{[]string{"python3", "-c", `print("hi there")`}, `'python3' '-c' 'print("hi there")'`},
		{[]string{"sh", "-c", "echo 'a b'"}, `'sh' '-c' 'echo '\''a b'\'''`},
		{[]string{"echo", "*"}, `'echo' '*'`},
	} {
		if got := shellJoin(tc.in); got != tc.want {
			t.Errorf("shellJoin(%q)\n got %s\nwant %s", tc.in, got, tc.want)
		}
	}
}
