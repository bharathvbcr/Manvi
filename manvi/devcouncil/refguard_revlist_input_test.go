package devcouncil

import (
	"strings"
	"testing"
)

// git before 2.42 reads only revisions from standard input, so an option line
// such as --not is an error there. The input has to be plain revisions: the tip,
// then each excluded id as ^<id>.
func TestNewCommitsStdinHasNoOptionLines(t *testing.T) {
	recorded := []string{"aaaa1111", "BBBB2222", "cccc3333"}
	in := revListInput("deadbeef", recorded)
	lines := strings.Split(strings.TrimSuffix(in, "\n"), "\n")
	if len(lines) != 1+len(recorded) || lines[0] != "deadbeef^{commit}" {
		t.Fatalf("unexpected input %q", in)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "-") {
			t.Errorf("an option line on standard input: %q", line)
		}
	}
	for i, id := range recorded {
		if lines[i+1] != "^"+id {
			t.Errorf("exclusion %d = %q, want %q", i, lines[i+1], "^"+id)
		}
	}
	if only := revListInput("deadbeef", nil); only != "deadbeef^{commit}\n" {
		t.Errorf("with nothing recorded: %q", only)
	}
}
