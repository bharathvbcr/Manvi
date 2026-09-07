package testsupport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/proc"
)

// The wire contract every Rust binary in the analysis plane is held to, in one
// place.
//
// It lived in dc/store's fuzz target, which is where it was written and where
// it caught the store panicking on an argument that was not valid UTF-8. When
// the searcher boundary arrived it needed exactly the same two helpers, and a
// second copy of an assertion is a second copy that can be relaxed on one side
// without anything noticing — so the assertion moved here rather than being
// duplicated. Every child boundary now answers to the same definition of a
// well-formed reply.

// ChildBound is how long a single child invocation may take.
//
// Every request these targets make is a local, bounded operation over a small
// file or a buffer already in memory, so this is slack rather than a budget:
// anything that reaches it is a child that will not return, which is the
// failure being looked for and not a slow machine.
const ChildBound = 60 * time.Second

// RunChild runs one binary to completion under ChildBound and returns its
// stdout and exit status.
//
// A child that does not finish is a test failure here rather than a returned
// error, because "it did not return" is the one outcome no caller of this
// boundary can recover from.
func RunChild(t testing.TB, binary string, args []string, stdin string) ([]byte, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ChildBound)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	// The same isolation the production clients apply, for the same reason and
	// with more force here: this helper is what the fuzz targets drive, so the
	// child it runs is the one most likely to be handed input that makes it
	// misbehave. A grandchild holding the stdout pipe would hang the fuzzer
	// rather than a turn, which is not better — it is the same failure with a
	// slower feedback loop.
	proc.ConfigureGroup(cmd)
	cmd.Stdin = strings.NewReader(stdin)
	// The same second bound the production clients set. Killing the process
	// does not unblock Wait while anything holds the stdout pipe.
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start)

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		// Exited zero.
	case errors.As(err, &exitErr):
		// Exited non-zero, which is a diagnosed outcome the caller asserts on.
	default:
		return nil, 0, err
	}
	if ctx.Err() != nil {
		t.Fatalf("%s %v did not return within %s (took %s); a boundary call that never "+
			"returns is a turn that never ends", filepath.Base(binary), args, ChildBound, elapsed)
	}
	return out, cmd.ProcessState.ExitCode(), nil
}

// AssertOneDiagnosedObject is the wire contract: one JSON object on stdout, and
// never a non-zero exit without a reason in it.
func AssertOneDiagnosedObject(t testing.TB, name string, args []string, stdout []byte, exitCode int) {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(string(stdout)))
	var first map[string]json.RawMessage
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("%s %v (exit %d) did not print one JSON object: %v (output %q)",
			name, args, exitCode, err, stdout)
	}
	if first == nil {
		t.Fatalf("%s %v (exit %d) printed a JSON null, which decodes into an empty reply "+
			"indistinguishable from one that carried no data", name, args, exitCode)
	}
	// Exactly one. A second object desynchronises a consumer that unmarshals
	// the whole stream, and this is the only place that can catch a producer
	// that starts printing two.
	if _, err := dec.Token(); err != io.EOF {
		rest, _ := io.ReadAll(dec.Buffered())
		t.Fatalf("%s %v (exit %d) printed more than one JSON value; the trailing bytes were %q (whole output %q)",
			name, args, exitCode, strings.TrimSpace(string(rest)), stdout)
	}

	if _, present := first["ok"]; !present {
		t.Fatalf("%s %v (exit %d) printed an object with no \"ok\" field: %q; a reply whose "+
			"outcome is absent decodes to ok:false and reads as a refusal nobody made",
			name, args, exitCode, stdout)
	}
	if exitCode == 0 {
		return
	}
	// A non-zero exit is only usable if it carries its own diagnosis: the Go
	// clients report "failed: exit status N" with nothing else when it does not.
	var failure struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(stdout, &failure); err != nil {
		t.Fatalf("%s %v exited %d and its output does not decode: %v (%q)", name, args, exitCode, err, stdout)
	}
	if failure.OK {
		t.Fatalf("%s %v exited %d while reporting ok:true (%q)", name, args, exitCode, stdout)
	}
	if strings.TrimSpace(failure.Error) == "" {
		t.Fatalf("%s %v exited %d with no error to report (%q); an exit code with no "+
			"diagnosis leaves the caller with nothing to tell an operator", name, args, exitCode, stdout)
	}
}
