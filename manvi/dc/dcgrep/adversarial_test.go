package dcgrep

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This boundary is the store's and the devmap's third sibling: one fork/exec
// per search, the answer as a single JSON document on stdout. What makes it
// worth its own adversarial pass is the shape of its success case. A store
// query that comes back wrong fails loudly at the next lease check; a search
// that comes back wrong is an empty list, and an empty list is a *valid,
// meaningful answer* that a model will act on.
//
// So every test below drives a binary that misbehaves in one specific way, and
// asserts the same thing: the client returned an error. Not a Result with
// Count zero — an error.

// fake writes an executable standing in for dcgrep.
func fake(t *testing.T, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake binaries here are shell scripts")
	}
	path := filepath.Join(t.TempDir(), name)
	// #nosec G306 -- this writes a shell script the test then execs, so the
	// owner execute bit is the point; no mode at or below 0600 would work.
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func searchWith(t *testing.T, binary string) (*Result, error) {
	t.Helper()
	client := New(binary, t.TempDir())
	client.Timeout = 5 * time.Second
	return client.Search(context.Background(), Request{Pattern: "needle"})
}

// TestEveryWayTheSearcherCanMisbehaveIsAnErrorNotAnEmptyResult is the whole
// contract in one table. Each binary below is a different way for the boundary
// to fail, and not one of them may produce a Result the caller can read as "the
// repository does not contain this".
func TestEveryWayTheSearcherCanMisbehaveIsAnErrorNotAnEmptyResult(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"output that is not JSON", "#!/bin/sh\necho 'this is not json'\n"},
		{"nothing at all on stdout", "#!/bin/sh\nexit 0\n"},
		{"a non-zero exit with no explanation", "#!/bin/sh\nexit 1\n"},
		{"the searcher's own error reply", `#!/bin/sh
echo '{"ok":false,"error":"pattern is not a valid regular expression"}'
exit 2
`},
		{"an error reply with no reason given", "#!/bin/sh\necho '{\"ok\":false}'\nexit 2\n"},
		// The nastiest of the set: valid JSON, exit 0, and a match list that is
		// empty because the field is absent rather than because nothing matched.
		// It is refused for the OK flag alone, which is why that flag is not
		// merely decorative.
		{"a well-formed reply that never says ok", `#!/bin/sh
echo '{"count":0,"matches":[]}'
`},
		{"a JSON array where an object belongs", "#!/bin/sh\necho '[]'\n"},
		{"a JSON null", "#!/bin/sh\necho 'null'\n"},
		{"a truncated document", "#!/bin/sh\nprintf '{\"ok\":true,\"count\":0,\"matc'\n"},
		{"two documents where one belongs", `#!/bin/sh
echo '{"ok":true,"count":0,"matches":[]}{"ok":true,"count":99}'
`},
		{"a flood that never ends", "#!/bin/sh\nexec yes '{\"ok\":true,\"count\":0,\"matches\":[]}'\n"},
		{"a binary that crashes on a signal", "#!/bin/sh\nkill -9 $$\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := searchWith(t, fake(t, "fake.sh", tc.body))
			if err == nil {
				t.Fatalf("must be an error, got a result the caller would read as a real answer: %+v", result)
			}
			if result != nil {
				t.Fatalf("an error must not also carry a result: %+v", result)
			}
		})
	}
}

// TestNullAndTrueDecodeWithoutErrorWhichIsWhyTheOKFlagIsChecked records why the
// two cheapest-looking cases above are in that table.
//
// `null` and `[]` both unmarshal into the result struct without error, leaving
// every field zeroed — which is byte-for-byte the shape of a search that ran
// and matched nothing. Nothing about the decode step can tell them apart. The
// OK flag is the only thing that does.
func TestNullDecodesIntoTheSameShapeAsARealNegative(t *testing.T) {
	empty, err := searchWith(t, fake(t, "empty.sh",
		"#!/bin/sh\necho '{\"ok\":true,\"count\":0,\"matches\":[]}'\n"))
	if err != nil {
		t.Fatalf("a genuine empty result is not an error: %v", err)
	}
	if empty.Count != 0 || len(empty.Matches) != 0 {
		t.Fatalf("expected a real negative: %+v", empty)
	}

	// Same zeroed struct, different meaning, and the client must separate them.
	if _, err := searchWith(t, fake(t, "null.sh", "#!/bin/sh\necho 'null'\n")); err == nil {
		t.Fatal("a null document must not pass as a search that found nothing")
	}
}

// TestAWedgedSearcherIsBoundedRatherThanWaitedOn covers the failure the other
// two process boundaries in this repository each hit once: a child that never
// returns, holding the turn open for as long as it feels like.
func TestAWedgedSearcherIsBoundedRatherThanWaitedOn(t *testing.T) {
	client := New(fake(t, "hang.sh", "#!/bin/sh\nsleep 60\n"), t.TempDir())
	client.Timeout = 200 * time.Millisecond

	start := time.Now()
	_, err := client.Search(context.Background(), Request{Pattern: "needle"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a searcher that never answers must be an error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("the error must name the timeout, got %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("took %s to honour a 200ms bound", elapsed)
	}
}

// TestACancelledContextStopsTheSearch keeps cancellation propagating across the
// boundary rather than stopping at it.
func TestACancelledContextStopsTheSearch(t *testing.T) {
	client := New(fake(t, "hang.sh", "#!/bin/sh\nsleep 60\n"), t.TempDir())
	client.Timeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	start := time.Now()
	if _, err := client.Search(ctx, Request{Pattern: "needle"}); err == nil {
		t.Fatal("a cancelled search must not return a result")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancellation took %s to take effect", elapsed)
	}
}

// TestNoBinaryIsItsOwnError so a caller can tell "not built" from "misbehaved"
// and name the remedy rather than the symptom.
func TestNoBinaryIsItsOwnError(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.Search(context.Background(), Request{Pattern: "x"}); !errors.Is(err, ErrNoBinary) {
		t.Fatalf("a nil client must report ErrNoBinary, got %v", err)
	}
	if _, err := New("", "/tmp").Search(context.Background(), Request{Pattern: "x"}); !errors.Is(err, ErrNoBinary) {
		t.Fatalf("an unconfigured client must report ErrNoBinary, got %v", err)
	}
	if err := New("", "/tmp").Available(context.Background()); !errors.Is(err, ErrNoBinary) {
		t.Fatalf("Available must agree with Search about what is configured, got %v", err)
	}
}

// TestAvailableRequiresAPositiveIdentification is the lesson the store boundary
// already learned and recorded: a probe that accepts anything answering
// {"ok":true} will certify a mock, a stale build, or an unrelated program that
// happens to sit at the configured path.
func TestAvailableRequiresAPositiveIdentification(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"a bare ok", "#!/bin/sh\necho '{\"ok\":true}'\n"},
		{"some other program", "#!/bin/sh\necho '{\"ok\":true,\"searcher\":\"ripgrep\",\"schema_version\":1}'\n"},
		{"a schema this build does not speak", "#!/bin/sh\necho '{\"ok\":true,\"searcher\":\"dc-grep\",\"schema_version\":99}'\n"},
		{"ok:false with the right identity", "#!/bin/sh\necho '{\"ok\":false,\"searcher\":\"dc-grep\",\"schema_version\":1}'\n"},
		{"nothing at all", "#!/bin/sh\nexit 0\n"},
		{"not JSON", "#!/bin/sh\necho hello\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := New(fake(t, "health.sh", tc.body), t.TempDir())
			if err := client.Available(context.Background()); err == nil {
				t.Fatal("must not certify this binary as a working searcher")
			}
		})
	}

	good := New(fake(t, "good.sh",
		"#!/bin/sh\necho '{\"ok\":true,\"searcher\":\"dc-grep\",\"schema_version\":1}'\n"), t.TempDir())
	if err := good.Available(context.Background()); err != nil {
		t.Fatalf("a correct handshake must pass, got %v", err)
	}
}

// TestTheOutputCapStopsAFloodWithoutKillingAWellBehavedChild pins both halves
// of cappedBuffer.
//
// The cap has to hold, or one runaway child allocates until the process dies.
// And the writer has to report a complete write while dropping bytes, or
// io.Copy turns the cap into io.ErrShortWrite, os/exec closes the pipe, and the
// child takes SIGPIPE — which is how a search that ran perfectly comes back
// reported as a failure. That exact defect is already in this repository's
// hardening ledger, from limitWriter.
func TestTheOutputCapStopsAFloodWithoutKillingAWellBehavedChild(t *testing.T) {
	buf := &cappedBuffer{limit: 10}

	n, err := buf.Write([]byte("12345"))
	if n != 5 || err != nil {
		t.Fatalf("a write under the cap must pass through: n=%d err=%v", n, err)
	}
	if buf.overflow {
		t.Fatal("nothing was dropped yet")
	}

	// Straddles the cap: five bytes fit, five do not, and the caller must still
	// be told all ten were written.
	n, err = buf.Write([]byte("6789abcde"))
	if n != 9 || err != nil {
		t.Fatalf("a straddling write must report complete: n=%d err=%v", n, err)
	}
	if !buf.overflow {
		t.Fatal("dropping bytes must be recorded")
	}
	if buf.buf.Len() != 10 {
		t.Fatalf("the cap must hold, buffered %d bytes", buf.buf.Len())
	}

	n, err = buf.Write([]byte("more"))
	if n != 4 || err != nil {
		t.Fatalf("a write past a full buffer must still report complete: n=%d err=%v", n, err)
	}
	if buf.buf.Len() != 10 {
		t.Fatalf("the cap must keep holding, buffered %d bytes", buf.buf.Len())
	}
}

// TestTheRequestReachesTheSearcherIntact is the positive control for the whole
// file. Every test above proves the client rejects a bad reply; this one proves
// it is actually sending the search, so none of the others is passing because
// nothing happens at all.
func TestTheRequestReachesTheSearcherIntact(t *testing.T) {
	root := t.TempDir()
	// Echoes the request back inside a well-formed reply, as the matched line.
	echo := fake(t, "echo.sh", `#!/bin/sh
body=$(cat)
printf '{"ok":true,"count":1,"matches":[{"path":"echo","line_number":1,"line":%s}]}\n' \
  "$(printf '%s' "$body" | sed 's/\\/\\\\/g; s/"/\\"/g; s/^/"/; s/$/"/')"
`)
	client := New(echo, root)
	result, err := client.Search(context.Background(), Request{
		Pattern:         "needle",
		Path:            "src",
		MaxResults:      7,
		IncludeIgnored:  true,
		CaseInsensitive: true,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	sent := result.Matches[0].Line
	for _, want := range []string{
		`"pattern":"needle"`,
		`"path":"src"`,
		`"max_results":7`,
		`"include_ignored":true`,
		`"case_insensitive":true`,
		`"root":"` + root + `"`,
	} {
		if !strings.Contains(sent, want) {
			t.Fatalf("the request did not carry %s: %s", want, sent)
		}
	}
}

// TestAnOversizedReplyIsRefusedRatherThanTruncatedIntoAnAnswer is the branch a
// 16 MiB fixture would otherwise be needed to reach.
//
// It matters because a truncated reply can still parse. Cut a valid result
// after its tenth match and what remains is JSON whose match array simply ends
// early — a capped sample wearing the shape of a complete answer.
func TestAnOversizedReplyIsRefusedRatherThanTruncatedIntoAnAnswer(t *testing.T) {
	client := New(fake(t, "big.sh",
		"#!/bin/sh\necho '{\"ok\":true,\"count\":1,\"matches\":[{\"path\":\"a\",\"line_number\":1,\"line\":\"needle\"}]}'\n"),
		t.TempDir())
	client.maxOutput = 8 // smaller than any well-formed reply

	_, err := client.Search(context.Background(), Request{Pattern: "needle"})
	if err == nil {
		t.Fatal("a reply over the cap must be refused")
	}
	if !strings.Contains(err.Error(), "more than 8 bytes") {
		t.Fatalf("the error must name the bound it exceeded, got %v", err)
	}
}

// TestAClientBuiltWithoutNewIsStillBounded covers the zero value. A cap that
// defaults to "none" is the failure the cap exists to prevent.
func TestAClientBuiltWithoutNewIsStillBounded(t *testing.T) {
	bare := &Client{}
	if bare.outputBound() != maxOutput || bare.stderrBound() != maxStderr {
		t.Fatalf("a zero-valued client must fall back to the package bounds, got %d/%d",
			bare.outputBound(), bare.stderrBound())
	}
}

// TestSkippedTotalCountsEveryReason decides whether the caller reports the
// coverage note at all, so a reason missing from the sum is a file silently
// dropped from a warning that exists to say files were dropped.
func TestSkippedTotalCountsEveryReason(t *testing.T) {
	if got := (Skipped{}).Total(); got != 0 {
		t.Fatalf("nothing skipped must total zero, got %d", got)
	}
	for _, tc := range []struct {
		name string
		s    Skipped
	}{
		{"too large", Skipped{TooLarge: 1}},
		{"binary", Skipped{Binary: 1}},
		{"unreadable", Skipped{Unreadable: 1}},
	} {
		if got := tc.s.Total(); got != 1 {
			t.Fatalf("a %s skip must reach the total, got %d", tc.name, got)
		}
	}
	if got := (Skipped{TooLarge: 2, Binary: 3, Unreadable: 4}).Total(); got != 9 {
		t.Fatalf("the total must be the sum, got %d", got)
	}
}
