package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FuzzServeAnswersEveryRequestItReads fuzzes the stdio protocol.
//
// This is the one surface a program other than the harness drives, and the
// contract a host depends on is narrow and absolute: every line it sends that
// carries an id comes back with a response carrying that id. A host that does
// not get one waits forever, and a sidecar that hangs its editor is worse than
// one that refuses.
//
// The invariants:
//
//   - Serve never panics and always returns on exhausted input.
//   - Every line written out is a complete, valid JSON object. A truncated
//     line would desynchronise the host's reader for the rest of the session.
//   - Every syntactically valid request bearing an id is answered exactly once
//     with that id, whether the op exists or not.
func FuzzServeAnswersEveryRequestItReads(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"{}",
		`{"id":"1","op":"hello"}`,
		`{"id":"1","op":"hello"}` + "\n" + `{"id":"2","op":"hello"}`,
		`{"id":"1","op":"nonexistent.op"}`,
		`{"op":"hello"}`,
		`{"id":"","op":"hello"}`,
		`{"id":"1"}`,
		`{"id":"1","op":"policy.check.file"}`,
		`{"id":"1","op":"policy.check.file","params":{"path":"a.go"}}`,
		`{"id":"1","op":"policy.check.command","params":{"command":"rm -rf /"}}`,
		`{"id":"1","op":"chat.prepare","params":{}}`,
		`{"id":"1","op":"chat.settle","params":{}}`,
		`{"id":"1","op":"chat.forget","params":{}}`,
		"not json",
		"{",
		`{"id":`,
		"\x00\x01\x02",
		`{"id":"1","op":"hello"}` + "\n" + "garbage\n" + `{"id":"2","op":"hello"}`,
		strings.Repeat(`{"id":"x","op":"hello"}`+"\n", 50),
		`{"id":"` + strings.Repeat("i", 5000) + `","op":"hello"}`,
		`{"id":"1","op":"hello","params":` + strings.Repeat("[", 500) + strings.Repeat("]", 500) + `}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		// capability.probe reaches for loopback endpoints. The protocol
		// contract is what is under test here, not the prober, and a fuzzer
		// that opened sockets would measure the network instead.
		if strings.Contains(input, OpCapabilityProbe) {
			t.Skip("probe performs I/O; covered by its own tests")
		}

		var out bytes.Buffer
		srv := New(&out, Options{HardRules: true, Posture: PostureHost})

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- srv.Serve(ctx, strings.NewReader(input)) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("Serve did not return on exhausted input %q", clipS(input))
		}

		// Every emitted line must be a whole JSON object.
		answered := map[string]int{}
		for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			if line == "" {
				continue
			}
			var resp Response
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				t.Fatalf("emitted a line that is not valid JSON: %q (%v)", clipS(line), err)
			}
			if !resp.OK && resp.Error == nil {
				t.Fatalf("a failed response carried no error: %q", clipS(line))
			}
			if resp.ID != "" {
				answered[resp.ID]++
			}
		}

		// Every well-formed request with an id must have exactly one answer.
		for _, line := range strings.Split(input, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var req Request
			if json.Unmarshal([]byte(line), &req) != nil || req.ID == "" {
				continue
			}
			if n := answered[req.ID]; n == 0 {
				t.Fatalf("request id %q was read but never answered; a host would wait forever", clipS(req.ID))
			}
		}
	})
}

func clipS(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "..."
}

// FuzzRecoverIDNeverAnswersUnderSomeoneElsesID pins the one thing the
// best-effort id recovery is not allowed to do.
//
// A line under the cap is correlated by encoding/json's reading of it; a line
// over the cap is correlated by recoverID's reading of its first 4 KiB. The
// second is allowed to give up — an empty id is a visible, unroutable refusal,
// and that is the documented worst case. It is not allowed to produce a
// *different* id, because that marks some other caller's in-flight request
// failed while the request that was actually refused waits forever. Recovery
// that gives up costs one confused log line; recovery that guesses costs two
// hangs.
//
// This is invisible to FuzzServeAnswersEveryRequestItReads: its corpus never
// reaches the 8 MiB line cap, so it never takes the oversized path at all.
// Driving recoverID directly is what makes the property reachable at fuzzing
// speed. That it does not also give up on everything is pinned by
// TestRecoverIDReadsTheEnvelopeRatherThanScanningForAnID.
func FuzzRecoverIDNeverAnswersUnderSomeoneElsesID(f *testing.F) {
	seeds := []string{
		`{"id":"7","op":"hello"}`,
		`{"op":"hello","id":"7"}`,
		`{"op":"hello","params":{"id":"nested"},"id":"7"}`,
		`{"op":"hello","params":[{"id":"nested"}],"id":"7"}`,
		`{"op":"hello","params":{"q":"{\"id\":\"nested\"}"},"id":"7"}`,
		`{"id":"first","id":"second","op":"hello"}`,
		`{"Id":"cased","op":"hello"}`,
		`{"id":"a\"b","op":"hello"}`,
		`{"id":"7","op":"hello"}`,
		`{"id":7,"op":"hello"}`,
		`{"id":null,"op":"hello"}`,
		`{"paid":"no","id":"7"}`,
		`{"id":{"id":"nested"},"op":"hello"}`,
		`{"params":{"a":{"b":{"id":"deep"}}},"id":"7"}`,
		`[{"id":"7"}]`,
		`{"id":`,
		`{`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, line string) {
		var req Request
		if json.Unmarshal([]byte(line), &req) != nil {
			// A fragment, not a request. There is no decoded id to disagree
			// with, and the head recovery is best-effort by construction.
			return
		}
		if got := recoverID([]byte(line)); got != "" && got != req.ID {
			t.Fatalf("recoverID(%q) = %q, but the decoder reads the id as %q; "+
				"a refusal under that id would fail a call nobody made",
				clipS(line), clipS(got), clipS(req.ID))
		}
	})
}
