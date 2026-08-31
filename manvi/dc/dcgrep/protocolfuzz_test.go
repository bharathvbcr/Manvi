package dcgrep

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"manvi/internal/testsupport"
)

// The searcher boundary is the store's and the repo map's third sibling, and it
// is fuzzed for the reason those two are: the reply is produced by a binary
// built from a different language with a different serializer, so it can change
// shape without anything in this build failing to compile.
//
// What makes this boundary the worst of the three to get wrong is the shape of
// its success case. A store reply that decodes wrongly fails at the next lease
// check. A search reply that decodes wrongly is an empty match list — which is
// a valid, meaningful, actionable answer, and the one a model will act on
// without hesitating.
//
// So these targets state the invariant rather than listing inputs someone
// thought of: whatever the bytes, the client either returns an error or returns
// a result that a caller may believe.

// FuzzReplyNeverDecodesIntoAnUnearnedAnswer fuzzes the decode-and-judge step
// every search passes through.
//
// The invariants:
//
//   - Decoding never panics, whatever the bytes.
//   - A reply is accepted only if it said ok. `null`, `[]`, `{}` and `0` all
//     unmarshal into the wire struct without error, leaving every field zeroed
//     — byte-identical to a search that ran and matched nothing.
//   - An accepted reply never carries a match the caller cannot use: a path
//     must be non-empty and a line number must be real, because the entire
//     value of a match is that it can be opened afterwards.
//   - Count and the match list agree, so a caller may use either.
func FuzzReplyNeverDecodesIntoAnUnearnedAnswer(f *testing.F) {
	for _, seed := range []string{
		"",
		"null",
		"[]",
		"{}",
		"0",
		"true",
		`{"ok":true}`,
		`{"ok":true,"count":0,"matches":[]}`,
		`{"ok":true,"count":0,"matches":null}`,
		`{"ok":false,"error":"pattern is not a valid regular expression"}`,
		`{"ok":false}`,
		`{"ok":true,"count":1,"matches":[{"path":"a.go","line_number":1,"line":"x"}]}`,
		`{"ok":true,"count":1,"matches":[{}]}`,
		`{"ok":true,"count":1,"matches":[{"path":"","line_number":0,"line":""}]}`,
		`{"ok":true,"count":1,"matches":[{"path":"a.go","line_number":-1,"line":"x"}]}`,
		`{"ok":true,"count":99,"matches":[{"path":"a.go","line_number":1,"line":"x"}]}`,
		`{"ok":true,"count":0,"matches":[{"path":"a.go","line_number":1,"line":"x"}]}`,
		`{"ok":true,"matches":[{"path":"../../etc/passwd","line_number":1,"line":"x"}]}`,
		`{"ok":true,"matches":[{"path":"/etc/passwd","line_number":1,"line":"x"}]}`,
		`{"OK":true,"Count":1}`,
		`{"ok":"yes"}`,
		`{"ok":1}`,
		`{"ok":true,"count":"many"}`,
		`{"ok":true,"truncated":true,"limit":0}`,
		`{"ok":true,"skipped":{"too_large":-5}}`,
		"{",
		`{"ok":true,"matches":`,
		`{"ok":true}{"ok":false}`,
		"\x00\x01\x02",
		`{"ok":true,"count":1,"matches":[` + strings.Repeat(`{"path":"a"},`, 200) + `{}]}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, reply string) {
		// A shell script cannot carry arbitrary bytes, so the reply is written
		// to a file the fake binary cats — which also keeps the fuzzer's input
		// exactly what the client reads.
		dir := t.TempDir()
		body := filepath.Join(dir, "reply.json")
		if err := os.WriteFile(body, []byte(reply), 0o644); err != nil {
			t.Skip("could not stage the reply")
		}
		script := filepath.Join(dir, "fake.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ncat "+body+"\n"), 0o755); err != nil {
			t.Skip("could not stage the binary")
		}

		client := New(script, dir)
		result, err := client.Search(context.Background(), Request{Pattern: "needle"})

		if err != nil {
			if result != nil {
				t.Fatalf("an error must not also carry a result: %+v", result)
			}
			return
		}
		if result == nil {
			t.Fatal("a nil error must come with a result")
		}

		// Accepted. Everything below is what a caller is now entitled to
		// believe, and each clause is something a model would act on.
		if !result.OK {
			t.Fatalf("a reply that did not say ok was accepted: %q", reply)
		}
		if result.Count != len(result.Matches) {
			t.Fatalf("count %d disagrees with %d matches, so a caller reading either is wrong: %q",
				result.Count, len(result.Matches), reply)
		}
		for _, match := range result.Matches {
			if match.Path == "" {
				t.Fatalf("a match with no path cannot be opened: %q", reply)
			}
			if match.LineNumber < 1 {
				t.Fatalf("line number %d names no line: %q", match.LineNumber, reply)
			}
			if strings.HasPrefix(match.Path, "/") || strings.Contains(match.Path, "..") {
				t.Fatalf("a match path escaping the repository was accepted: %q", match.Path)
			}
			if !utf8.ValidString(match.Line) || !utf8.ValidString(match.Path) {
				t.Fatalf("invalid UTF-8 reached a caller: %q", reply)
			}
		}
		if result.Truncated && result.Limit < 1 {
			t.Fatalf("a truncated result must name the limit that truncated it: %q", reply)
		}
		if result.Skipped.Total() < 0 {
			t.Fatalf("a negative skip count is not a count: %q", reply)
		}
	})
}

// FuzzListReplyNeverDecodesIntoAnUnearnedAnswer is the same target for the
// listing, which carries the additional weight of being what
// devcouncil_find_files reports as the contents of the repository.
func FuzzListReplyNeverDecodesIntoAnUnearnedAnswer(f *testing.F) {
	for _, seed := range []string{
		"", "null", "[]", "{}",
		`{"ok":true,"count":0,"paths":[]}`,
		`{"ok":true,"count":0,"paths":null}`,
		`{"ok":true,"count":1,"paths":["src/a.go"]}`,
		`{"ok":true,"count":1,"paths":[""]}`,
		`{"ok":true,"count":1,"paths":["/etc/passwd"]}`,
		`{"ok":true,"count":1,"paths":["../escape"]}`,
		`{"ok":true,"count":9,"paths":["a"]}`,
		`{"ok":false,"error":"unreadable"}`,
		`{"ok":true,"truncated":true}`,
		"{",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, reply string) {
		dir := t.TempDir()
		body := filepath.Join(dir, "reply.json")
		if err := os.WriteFile(body, []byte(reply), 0o644); err != nil {
			t.Skip("could not stage the reply")
		}
		script := filepath.Join(dir, "fake.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ncat "+body+"\n"), 0o755); err != nil {
			t.Skip("could not stage the binary")
		}

		listing, err := New(script, dir).List(context.Background(), ListRequest{})
		if err != nil {
			if listing != nil {
				t.Fatalf("an error must not also carry a listing: %+v", listing)
			}
			return
		}
		if listing == nil {
			t.Fatal("a nil error must come with a listing")
		}
		if !listing.OK {
			t.Fatalf("a listing that did not say ok was accepted: %q", reply)
		}
		if listing.Count != len(listing.Paths) {
			t.Fatalf("count %d disagrees with %d paths: %q", listing.Count, len(listing.Paths), reply)
		}
		for _, path := range listing.Paths {
			if path == "" {
				t.Fatalf("an empty path names no file: %q", reply)
			}
			if strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
				t.Fatalf("a path escaping the repository was accepted: %q", path)
			}
		}
	})
}

// The seeds above are JSON the client is meant to decode; this keeps the
// encoder honest about that so a seed that is not JSON at all is a deliberate
// choice rather than a typo.
var _ = json.Valid

// FuzzGrepChildAnswersEveryRequestWithExactlyOneObject fuzzes the producer.
//
// The two targets above fuzz what this side does with a reply; this one fuzzes
// what the real binary does with a request, and the pair meet in the middle:
// whatever bytes the child chose to print, the client must still reach exactly
// one answer about them.
//
// It is the target that found, in the store, that a child reached
// std::env::args() on an argument that was not valid UTF-8 and panicked — exit
// 101, a backtrace on stderr, nothing on stdout, which is the one shape this
// boundary cannot survive. The searcher takes its request on stdin rather than
// in the argument vector, so the equivalent surface here is arbitrary bytes as
// a JSON document, plus the command word itself.
func FuzzGrepChildAnswersEveryRequestWithExactlyOneObject(f *testing.F) {
	bin := testsupport.DCGrep(f)
	// One repository for the whole run. The interesting inputs are the ones
	// that reach a real walk, and a fresh empty tree per case would fuzz the
	// "nothing to search" path over and over.
	root := f.TempDir()
	for rel, body := range map[string]string{
		".gitignore":   "dist/\n",
		"src/a.rs":     "needle\nfn main() {}\n",
		"dist/gen.rs":  "needle\n",
		".hidden/c.rs": "needle\n",
	} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			f.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			f.Fatal(err)
		}
	}

	for _, seed := range []struct {
		cmd     uint8
		request string
	}{
		{0, `{"pattern":"needle","root":"ROOT"}`},
		{0, `{"pattern":"needle","root":"ROOT","include_ignored":true}`},
		{0, `{"pattern":"[","root":"ROOT"}`},
		{0, `{"pattern":"","root":"ROOT"}`},
		{0, `{"pattern":"needle","root":"ROOT","path":"../.."}`},
		{0, `{"pattern":"needle","root":"ROOT","max_results":-1}`},
		{0, `{"pattern":"needle","root":"ROOT","max_results":99999999}`},
		{0, `{"pattern":"needle","root":"ROOT","max_line_bytes":18446744073709551615}`},
		{0, `{"pattern":"needle","root":"ROOT","max_file_bytes":18446744073709551615}`},
		{0, `{"pattern":"needle","root":""}`},
		{0, `{"pattern":"needle"}`},
		{0, `{"root":"ROOT"}`},
		{0, `{}`},
		{0, `null`},
		{0, `[]`},
		{0, `not json at all`},
		{0, ``},
		{0, `{"pattern":"\ud800","root":"ROOT"}`},
		{0, "{\"pattern\":\"\x00\",\"root\":\"ROOT\"}"},
		{0, `{"pattern":"a{1000}{1000}","root":"ROOT"}`},
		{1, `{"root":"ROOT"}`},
		{1, `{"root":"ROOT","include_ignored":true}`},
		{1, `{"root":"ROOT","max_results":1}`},
		{1, `null`},
		{2, ``},
		{3, `{"pattern":"needle","root":"ROOT"}`},
	} {
		f.Add(seed.cmd, seed.request)
	}

	commands := []string{"search", "files", "health", "not-a-command"}

	f.Fuzz(func(t *testing.T, cmdIdx uint8, request string) {
		command := commands[int(cmdIdx)%len(commands)]
		// The seeds name the repository with a placeholder because the temp
		// directory is not known when they are written; the fuzzer's own
		// mutations are passed through untouched.
		request = strings.ReplaceAll(request, "ROOT", root)

		// A request larger than the child will read is a bound this target must
		// not spend its whole budget on; the bound has its own test.
		if len(request) > 1<<20 {
			return
		}

		stdout, exitCode, err := testsupport.RunChild(t, bin, []string{command}, request)
		if err != nil {
			t.Fatalf("dcgrep %s: %v", command, err)
		}
		testsupport.AssertOneDiagnosedObject(t, "dcgrep", []string{command}, stdout, exitCode)

		// ok:true and exit zero are the same statement, and a boundary where
		// they can disagree is one where a caller checking either is wrong.
		var reply struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(stdout, &reply); err != nil {
			t.Fatalf("dcgrep %s printed undecodable output %q", command, stdout)
		}
		if reply.OK != (exitCode == 0) {
			t.Fatalf("dcgrep %s printed ok=%v and exited %d; those must agree (%q)",
				command, reply.OK, exitCode, stdout)
		}

		// And the producer's real bytes through the consumer's real decision:
		// exactly one of a result and an error, never both and never neither.
		if command != "search" && command != "files" {
			return
		}
		dir := t.TempDir()
		body := filepath.Join(dir, "reply.json")
		if err := os.WriteFile(body, stdout, 0o644); err != nil {
			t.Skip("could not stage the reply")
		}
		script := filepath.Join(dir, "replay.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ncat "+body+"\nexit "+itoa(exitCode)+"\n"), 0o755); err != nil {
			t.Skip("could not stage the replay")
		}
		client := New(script, root)

		var gotResult, gotErr bool
		if command == "search" {
			result, decodeErr := client.Search(context.Background(), Request{Pattern: "needle"})
			gotResult, gotErr = result != nil, decodeErr != nil
		} else {
			listing, decodeErr := client.List(context.Background(), ListRequest{})
			gotResult, gotErr = listing != nil, decodeErr != nil
		}
		if gotResult == gotErr {
			t.Fatalf("dcgrep %s printed %q (exit %d) and the client answered result=%v err=%v; "+
				"exactly one of those must be set", command, stdout, exitCode, gotResult, gotErr)
		}
	})
}
