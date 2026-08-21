package devmap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeBinary writes an executable script and returns its path.
func writeBinary(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// countingDevmap is a devmap whose --help reply is configurable and whose
// invocations append one line to an argv journal, so tests can tell whether
// the client re-ran it at all.
func countingDevmap(t *testing.T, dir, helpText string) (binaryPath, argvLog string) {
	t.Helper()
	argvLog = filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argvLog + "\nprintf '\\036\\n' >> " + argvLog + "\n" +
		"case \" $* \" in *\" --help \"*) cat <<'HELP'\n" + helpText + "\nHELP\nexit 0;;\nesac\n" +
		"echo '{}'\n"
	return writeBinary(t, dir, "devmap", script), argvLog
}

func argvCount(t *testing.T, log string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	// One \036 record separator per invocation; three argument lines each.
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "\x1e" {
			count++
		}
	}
	return count
}

// A current binary passes the probe once; every later call reuses the verdict
// instead of paying a subcommand per query.
func TestAHealthyProbeIsCachedForTheClientLife(t *testing.T) {
	dir := t.TempDir()
	bin, log := countingDevmap(t, dir,
		"usage: devmap manifest [OPTIONS]\n      --graph-output <PATH>")
	c := New(bin, dir)

	for i := 0; i < 3; i++ {
		if err := c.Probe(context.Background()); err != nil {
			t.Fatalf("probe %d failed: %v", i, err)
		}
	}

	runs := argvCount(t, log)
	if runs == 0 {
		t.Fatal("the probe never ran")
	}
	// Three Probes must answer from one execution.
	if runs > 1 {
		t.Fatalf("a healthy verdict was re-probed: %d executions for 3 calls", runs)
	}
}

// An outdated install is refused by capability, named as such, before any
// command can fail halfway through a long build.
func TestAnOutdatedBinaryIsRefusedByCapabilityNotVersion(t *testing.T) {
	dir := t.TempDir()
	bin := writeBinary(t, dir, "devmap",
		"#!/bin/sh\ncase \" $* \" in *\" --help \"*) echo 'usage: devmap 0.1.0'; exit 0;; esac\necho '{}'\n")
	c := New(bin, dir)

	err := c.Probe(context.Background())
	if err == nil {
		t.Fatal("a binary without --graph-output must be refused")
	}
	for _, want := range []string{"too old", "--graph-output", bin} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must name %q, got %v", want, err)
		}
	}

	if _, err := c.Available(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "repo map unavailable") {
		t.Fatalf("Available must phrase the refusal as unavailability, got %v", err)
	}
}

// A hung binary costs one bounded probe and then fails fast until the
// cooldown expires — the port of DevCouncil's measured 123s-per-call stall.
func TestAHungBinaryFailsFastWithinTheCooldown(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	bin := writeBinary(t, dir, "devmap",
		"#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+argvLog+"\nprintf '\\036\\n' >> "+argvLog+"\n"+
			"case \" $* \" in *\" --help \"*) sleep 60;; esac\necho '{}'\n")
	c := New(bin, dir)
	c.probeTimeout = 150 * time.Millisecond

	start := time.Now()
	err := c.Probe(context.Background())
	first := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "presumed hung") {
		t.Fatalf("want a hung-producer refusal, got %v", err)
	}
	if first > 5*time.Second {
		t.Fatalf("the probe was not bounded by its own timeout: %s", first)
	}

	start = time.Now()
	err2 := c.Probe(context.Background())
	fast := time.Since(start)
	if err2 == nil || !strings.Contains(err2.Error(), "presumed hung") {
		t.Fatalf("cooldown must return the same refusal, got %v", err2)
	}
	if fast > 50*time.Millisecond {
		t.Fatalf("fail-fast must not re-run the binary, took %s", fast)
	}
	if runs := argvCount(t, argvLog); runs != 1 {
		t.Fatalf("expected exactly one execution within the cooldown, got %d", runs)
	}
}

// After the cooldown the binary is asked again, so a rebuilt or replaced
// install is noticed without restarting the process.
func TestTheProbeRetriesAfterItsCooldown(t *testing.T) {
	dir := t.TempDir()
	bin, _ := countingDevmap(t, dir, "usage: devmap manifest\n      --graph-output <PATH>")
	c := New(bin, dir)
	c.probeTimeout = 200 * time.Millisecond

	// Seed a failure whose cooldown has already elapsed.
	c.probeMu.Lock()
	c.probeErr = context.DeadlineExceeded
	c.probedAt = time.Now().Add(-probeCooldown - time.Second)
	c.probeMu.Unlock()

	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("an expired cooldown must re-probe a healthy binary, got %v", err)
	}
}

// Build probes before spending the repository-sized work.
func TestBuildRefusesAnOldBinaryBeforeBuilding(t *testing.T) {
	dir := t.TempDir()
	bin := writeBinary(t, dir, "devmap",
		"#!/bin/sh\ncase \" $* \" in *\" --help \"*) echo 'usage: devmap 0.1.0'; exit 0;; esac\nsleep 120\necho '{}'\n")
	c := New(bin, dir)

	start := time.Now()
	_, err := c.Build(context.Background(), 500*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("Build must refuse an outdated binary, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the refusal must precede the build's own work, took %s", elapsed)
	}
}
