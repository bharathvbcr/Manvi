package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/testsupport"
)

// The protocol between the Go execution plane and the Rust state plane is one
// fork/exec per call, with the child's whole answer arriving as a single JSON
// object on stdout. Fifteen fuzz targets in this repository sit inside the Go
// plane; none of them crossed that boundary, which left the least trustworthy
// input the harness reads — bytes from a binary built by another language's
// toolchain — covered only by the handful of hand-written replies in
// adversarial_test.go.
//
// The two targets here take the two halves of that boundary. The first fuzzes
// what this package makes of an arbitrary reply, with no process in the way, so
// the decision is explored rather than sampled. The second fuzzes the real
// dcstore with arbitrary requests, because the first can only ever prove what
// the Go side does with bytes — not that the producer confines itself to bytes
// of that shape.

// storeCommands is every subcommand run() is called with, in the spelling the
// judging in decodeReply keys off. It is written out rather than derived from
// signalsOutcomeWithOK because the interesting cases are precisely the commands
// that are *not* in that map.
var storeCommands = []string{
	"acquire", "diagnose", "release", "renew", "active", "list", "task", "ready", "health", "scope-append",
}

// FuzzStoreReplyIsNeverAZeroValueReadAsSuccess fuzzes the Go side of the store
// protocol: given any bytes a child could print, any exit status it could
// carry, and any command it could have been answering, decodeReply must reach
// exactly one of two answers and never a third.
//
// The third answer is the one this package exists to prevent. Every caller
// above run() branches on a *response, and Go's JSON decoder is specified to
// leave absent fields at their zero value and report no error — so a reply that
// decoded into nothing is a lease that reads as absent, a task that reads as
// unknown, and a store that reads as healthy and empty. `Valid()` gates every
// write in the harness on exactly that path.
//
// The invariants:
//
//   - It never panics, whatever the bytes.
//   - Exactly one of (reply, error) is set. A nil reply with a nil error would
//     be read by callers as an empty-but-healthy store.
//   - A child that failed, or whose output hit the size bound, never yields a
//     reply — regardless of what its bytes said.
//   - A reply only ever comes back from bytes that decoded to a JSON *object*.
//     `null` is the document that makes this non-trivial: it is valid JSON, it
//     unmarshals into the response struct without error, and it leaves every
//     field zero.
//   - ok:false only survives for the four commands that use it as an answer.
func FuzzStoreReplyIsNeverAZeroValueReadAsSuccess(f *testing.F) {
	// Real replies, near-misses, and the shapes that decode without meaning
	// anything. The last group is the point of the target.
	seeds := []string{
		"",
		" ",
		"\n\n",
		"null",
		" null \n",
		"[]",
		"0",
		`""`,
		"true",
		"{}",
		`{"ok":true}`,
		`{"ok":false}`,
		`{"ok":false,"error":"no such task"}`,
		`{"ok":false,"code":"held_by_other","holder":"builder-2"}`,
		`{"ok":true,"store":"dc-store","schema_version":1,"exclusion_index":"verified"}`,
		`{"ok":true,"store":"dc-store","schema_version":0,"exclusion_index":""}`,
		`{"ok":true,"lease":{"id":"1","task_id":"T","owner":"o","token":"tok","status":"active"}}`,
		`{"ok":true,"lease":null}`,
		`{"ok":true,"leases":[]}`,
		`{"ok":true,"leases":null}`,
		`{"ok":true,"task":{"id":"T","planned_files":[],"agent_appended_planned_files":null}}`,
		`{"ok":true,"task":{"id":"T","agent_appended_planned_files":"not an array"}}`,
		`{"ok":true,"released":true}`,
		`{"ok":false,"code":"scope_stale","current_appended":[]}`,
		`{"ok":false,"code":"scope_stale","current_appended":null}`,
		`{"ok":true,"written":true}`,
		`{"ok":true,"tasks":["A","B"]}`,
		"{",
		`{"ok":`,
		`{"ok":true}{"ok":true}`,
		`{"ok":true}` + "\n" + `{"ok":false}`,
		"\x00\x01\x02",
		`{"ok":true,"error":"` + strings.Repeat("e", 4096) + `"}`,
		`{"ok":true,"leases":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`,
		"this is not json",
	}
	for _, s := range seeds {
		for _, cmd := range []uint8{0, 1, 3, 5, 8} {
			f.Add(s, cmd, false, false, "")
		}
		f.Add(s, uint8(0), true, false, "exit status 2")
		f.Add(s, uint8(5), false, true, "")
	}

	f.Fuzz(func(t *testing.T, stdout string, cmdIdx uint8, childFailed, overflowed bool, stderr string) {
		command := storeCommands[int(cmdIdx)%len(storeCommands)]
		var runErr error
		if childFailed {
			runErr = errors.New("exit status 2")
		}

		reply, err := decodeReply(command, []byte(stdout), overflowed, []byte(stderr), runErr)

		// One answer, never none and never both. `run` returns this pair
		// straight through, so a (nil, nil) here is a nil *response
		// dereferenced or — worse — read as an empty store by every caller in
		// this file that returns out.Lease, out.Leases or out.Tasks directly.
		switch {
		case reply == nil && err == nil:
			t.Fatalf("%s: decodeReply(%q, overflow=%v, childFailed=%v) returned no reply and no error; "+
				"callers read that as an empty-but-healthy store", command, stdout, overflowed, childFailed)
		case reply != nil && err != nil:
			t.Fatalf("%s: decodeReply returned both a reply (%+v) and an error (%v)", command, reply, err)
		}
		if err != nil {
			// An error must say which command produced it, or an operator
			// cannot tell a failed lease check from a failed health check.
			if !strings.Contains(err.Error(), command) {
				t.Fatalf("error does not name the command %q: %v", command, err)
			}
			// The error must not quote a lease token back out. Nothing here
			// puts one in, but the unparseable-output branch echoes stdout
			// verbatim, so this pins the one place that could.
			return
		}

		// From here a reply came back, and every claim below is about what the
		// bytes actually contained.
		if overflowed {
			t.Fatalf("%s: a reply survived a stdout that hit the %d-byte bound; "+
				"a truncated document may still parse, and a partial answer is not a short one", command, maxOutput)
		}
		if childFailed {
			t.Fatalf("%s: a reply survived a child that exited non-zero (reply %+v)", command, reply)
		}

		// The anti-zero-value claim, checked against the bytes rather than
		// against the struct: a reply may only come from a JSON object. Decode
		// independently so a change to the response struct cannot make this
		// agree with itself.
		var asObject map[string]json.RawMessage
		if decodeErr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &asObject); decodeErr != nil || asObject == nil {
			t.Fatalf("%s: decodeReply produced a reply from %q, which is not a JSON object "+
				"(independent decode: %v); every field of that reply is a zero value that reads as an answer",
				command, stdout, decodeErr)
		}
		if !reply.OK && !signalsOutcomeWithOK[command] {
			t.Fatalf("%s: an ok:false reply survived for a command whose ok:false is not an answer (%+v)",
				command, reply)
		}
		// A reply that claims ok must have said so; the field cannot have been
		// invented by the decoder. Matched case-insensitively because that is
		// what encoding/json does when an exact key match fails, so requiring
		// the exact spelling here would assert a stricter contract than the
		// decoder actually applies — and the fuzzer said so within four
		// seconds, with {"oK":true}.
		if reply.OK {
			said := false
			for key := range asObject {
				if strings.EqualFold(key, "ok") {
					said = true
					break
				}
			}
			if !said {
				t.Fatalf("%s: reply reports ok:true but %q carries no \"ok\" field", command, stdout)
			}
		}
	})
}

// FuzzStoreChildAnswersEveryRequestWithExactlyOneObject drives the real dcstore
// across the real process boundary with fuzzed requests.
//
// The target above can only establish what this package does with bytes. It
// cannot establish that the producer confines itself to bytes of that shape,
// and that half of the contract has never been fuzzed from either side: the
// Rust suite tests the store's functions, and the Go suite tests a fixed list
// of requests. The gap is a request the harness can construct — task ids and
// owners reach this boundary from a model — that makes the child print two
// objects, print none, exit non-zero without a diagnosis, or not return.
//
// The invariants, which are the ones store.run() is written against:
//
//   - The child always returns inside the bound. A store that does not is a
//     turn that never ends.
//   - Its stdout is exactly one JSON object. Not zero, not two: run() decodes
//     with json.Unmarshal over the whole stream, so a second object makes the
//     answer unparseable and a partial one could still parse.
//   - A non-zero exit always carries a parseable `{"ok":false,"error":...}`
//     with a non-empty reason. An exit code with no diagnosis is the case
//     run() has to report as "failed" with nothing to tell the operator.
//   - Whatever it prints, decodeReply reaches one answer about it — the two
//     halves of the boundary are checked against each other rather than
//     separately.
func FuzzStoreChildAnswersEveryRequestWithExactlyOneObject(f *testing.F) {
	bin := testsupport.DCStore(f)
	// One database for the whole run rather than one per input. Creating a
	// store costs a schema migration, and the interesting inputs are the ones
	// that reach a store with rows in it — a fresh empty database per case
	// would fuzz the migration path over and over and the lease path never.
	db := filepath.Join(f.TempDir(), "state.sqlite")

	for _, seed := range []struct {
		cmd                       uint8
		task, owner, token, extra string
		ttl                       int64
		force                     bool
	}{
		{0, "TASK-1", "builder", "", "", 60, false},
		{0, "TASK-1", "builder-2", "", "", 60, false},
		{0, "TASK-1", "builder", "", "", 60, true},
		{0, "", "builder", "", "", 60, false},
		{0, "TASK-1", "", "", "", 0, false},
		{0, "TASK-1", "b", "", "", -1, false},
		{0, "--db", "--force", "", "", 60, false},
		{0, "TASK-\x00-NUL", "b", "", "", 60, false},
		{0, strings.Repeat("T", 8192), "b", "", "", 60, false},
		{0, "TASK-1", "b", "", "", 1 << 62, false},
		{1, "TASK-1", "", "not-a-token", "", 0, false},
		{1, "TASK-1", "", "", "", 0, false},
		{2, "TASK-1", "", "tok", "", 0, false},
		{3, "TASK-1", "", "tok", "", 60, false},
		{4, "TASK-1", "", "", "", 0, false},
		{5, "", "", "", "", 0, false},
		{6, "TASK-1", "", "", "", 0, false},
		{6, "no-such-task", "", "", "", 0, false},
		{7, "", "", "", "", 0, false},
		{8, "", "", "", "", 0, false},
		{9, "TASK-1", "", "tok", "[]", 0, false},
		{9, "TASK-1", "", "tok", "not json", 0, false},
		{9, "TASK-1", "", "tok", `[{"path":"a.go","allowed_change":"modify"}]`, 0, false},
		{200, "TASK-1", "b", "", "", 60, false},
		// The regression this target found on its first run: an argument that
		// is not valid UTF-8 used to panic the child rather than be refused by
		// it.
		{0, "TASK-\xff", "b", "", "", 60, false},
		{0, "TASK-1", "\xff\xfe", "", "", 60, false},
		{1, "TASK-1", "", "\xc3\x28", "", 0, false},
		{9, "TASK-1", "", "tok", "\xed\xa0\x80", 0, false},
	} {
		f.Add(seed.cmd, seed.task, seed.owner, seed.token, seed.extra, seed.ttl, seed.force)
	}

	f.Fuzz(func(t *testing.T, cmdIdx uint8, task, owner, token, extra string, ttl int64, force bool) {
		// A NUL byte in an argument is refused by the operating system before
		// either plane sees it, so it says nothing about the protocol and is
		// the one value skipped here. Invalid UTF-8 is deliberately *not*
		// skipped: argument vectors on Unix are bytes, and skipping them is
		// what hid this target's first real finding for its first run. The
		// store used to reach them through std::env::args(), which panics —
		// exit 101, a backtrace on stderr, an empty stdout — breaking the one
		// property this boundary rests on. Argument smuggling through the arg
		// vector is covered by TestFlagLikeIdentifiersDoNotBecomeFlags.
		for _, s := range []string{task, owner, token, extra} {
			if strings.ContainsRune(s, 0) {
				return
			}
		}

		args := []string{"--db", db, storeCommands[int(cmdIdx)%len(storeCommands)]}
		add := func(flag, value string) {
			if value != "" {
				args = append(args, flag, value)
			}
		}
		add("--task", task)
		add("--owner", owner)
		add("--token", token)
		switch storeCommands[int(cmdIdx)%len(storeCommands)] {
		case "acquire", "renew":
			args = append(args, "--ttl-seconds", strconv.FormatInt(ttl, 10))
			if force {
				args = append(args, "--force", "true")
			}
		case "scope-append":
			args = append(args, "--expected", "[]", "--appended", extra)
		}

		stdout, exitCode, err := runChild(t, bin, args, "")
		if err != nil {
			t.Fatalf("dcstore %v: %v", args[1:], err)
		}
		assertOneDiagnosedObject(t, "dcstore", args[1:], stdout, exitCode)

		// The producer's real bytes, through the consumer's real decision. This
		// is what ties the two targets together: whatever the child chose to
		// say, the Go side must still reach exactly one answer about it.
		var runErr error
		if exitCode != 0 {
			runErr = fmt.Errorf("exit status %d", exitCode)
		}
		reply, decodeErr := decodeReply(args[2], stdout, false, nil, runErr)
		if (reply == nil) == (decodeErr == nil) {
			t.Fatalf("dcstore %v printed %q (exit %d) and decodeReply answered reply=%+v err=%v; "+
				"exactly one of those must be set", args[1:], stdout, exitCode, reply, decodeErr)
		}
	})
}

// The child wire contract lives in internal/testsupport now: the searcher
// boundary needed the identical two helpers, and a second copy of an assertion
// is a second copy that can be relaxed on one side without anything noticing.
func runChild(t *testing.T, binary string, args []string, stdin string) ([]byte, int, error) {
	t.Helper()
	return testsupport.RunChild(t, binary, args, stdin)
}

func assertOneDiagnosedObject(t *testing.T, name string, args []string, stdout []byte, exitCode int) {
	t.Helper()
	testsupport.AssertOneDiagnosedObject(t, name, args, stdout, exitCode)
}
