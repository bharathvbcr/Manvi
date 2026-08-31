package devmap

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The devmap boundary is the store boundary's twin: one fork/exec per query,
// the answer as a single JSON document on stdout, and — uniquely here — the
// other half of the answer as free text on stderr. Both halves are produced by
// a binary built from a different repository, so both can change shape without
// anything in this build failing to compile, and both have a documented history
// of a change of shape being read as an answer: Edge exists because dependency
// replies were decoded into a struct sharing none of their field names and
// `graph_context` reported "no dependencies" for a file with 166, and Notices
// exists because a build that refused half the tree was reported clean.
//
// assertShape and readNotices are the two guards that were written in response.
// Neither had been fuzzed. These targets state what they must do rather than
// listing what they did on the inputs someone thought of.

// FuzzDevmapReplyNeverDecodesToAConfidentEmptyAnswer fuzzes the decode-and-judge
// step every devmap query passes through.
//
// The invariants:
//
//   - Decoding never panics, whatever the bytes.
//   - A reply that decoded into items none of which carry an identifier is
//     always refused. That is the shape change that reads as "no results",
//     and "no results" from a repo map is what authorises a deletion.
//   - Conversely, a reply that is accepted always has at least one item this
//     build can identify — the guard must not pass by looking the other way.
//   - A document that is not a JSON object never yields items at all. `null`
//     and `[]` both unmarshal into the wire structs without error.
func FuzzDevmapReplyNeverDecodesToAConfidentEmptyAnswer(f *testing.F) {
	for _, seed := range []string{
		"",
		"null",
		"[]",
		"{}",
		`{"items":[],"hidden":0}`,
		`{"items":null}`,
		`{"items":[{}]}`,
		`{"items":[{},{},{}]}`,
		`{"items":[{"file_path":"a.go","symbol_name":"F"}]}`,
		`{"items":[{"source_file":"a.go","target_file":"b.go","edge_kind":"calls"}]}`,
		`{"items":[{"file_path":"a.go","symbol_name":"F"},{}]}`,
		`{"items":[{"unknown_field":"a.go"}]}`,
		`{"items":[{"filePath":"a.go","symbolName":"F"}]}`,
		`{"items":[{"file_path":"","symbol_name":""}]}`,
		`{"items":[{"file_path":null,"symbol_name":null}]}`,
		`{"items":[{"confidence":1.0,"is_exempt":true}]}`,
		`{"items":[{"file_path":"a.go"}],"hidden":-1}`,
		`{"items":[{"file_path":"a.go"}],"hidden":9223372036854775807}`,
		`{"db_path":"x","generation_id":4,"node_count":100,"is_fresh":true}`,
		`{"db_path":"x","generation_id":0,"node_count":0,"is_fresh":true}`,
		`{"generation_id":-1,"node_count":-1}`,
		`{"degraded_reason":"index is rebuilding"}`,
		`{"degraded_reason":null}`,
		"{",
		`{"items":`,
		`{"items":[{"file_path":"a.go"}]}{"items":[]}`,
		"\x00\x01\x02",
		`{"items":[` + strings.Repeat(`{},`, 500) + `{}]}`,
	} {
		for _, shape := range []uint8{0, 1, 2, 3} {
			f.Add(seed, shape)
		}
	}

	f.Fuzz(func(t *testing.T, payload string, shapeIdx uint8) {
		trimmed := strings.TrimSpace(payload)

		// The four wire shapes runQuery decodes into, each with the extract
		// function its caller passes and the command name it reports under.
		// Status is here because it is the one reply that is not a list, and a
		// decoder that cannot be asked about items still must not panic.
		switch shapeIdx % 4 {
		case 0:
			var wire searchResult
			if json.Unmarshal([]byte(trimmed), &wire) != nil {
				return
			}
			assertShapeIsHonest(t, "search", trimmed, wire.Items, func(s Symbol) bool { return s.identified() })
		case 1:
			var wire edgeResult
			if json.Unmarshal([]byte(trimmed), &wire) != nil {
				return
			}
			assertShapeIsHonest(t, "deps", trimmed, wire.Items, func(e Edge) bool { return e.identified() })
		case 2:
			var wire deadResult
			if json.Unmarshal([]byte(trimmed), &wire) != nil {
				return
			}
			assertShapeIsHonest(t, "dead", trimmed, wire.Items, func(d DeadSymbol) bool { return d.identified() })
		case 3:
			var status Status
			if json.Unmarshal([]byte(trimmed), &status) != nil {
				return
			}
			// The claim Available() rests on: a status that decoded out of a
			// document carrying none of its fields must never describe an
			// index that can answer questions. Both counts are required to be
			// positive there precisely because a zero-valued decode satisfies
			// neither, and a negative one is a producer this build cannot
			// reason about.
			if !isJSONObject(trimmed) && (status.GenerationID > 0 || status.NodeCount > 0) {
				t.Fatalf("%q is not a JSON object yet decoded to generation=%d nodes=%d",
					trimmed, status.GenerationID, status.NodeCount)
			}
			if status.DegradedReason != nil && !isJSONObject(trimmed) {
				t.Fatalf("%q is not a JSON object yet carried a degraded reason", trimmed)
			}
		}
	})
}

// assertShapeIsHonest holds assertShape to both directions of its contract: it
// must refuse a decode that produced nothing this build recognises, and it must
// not refuse one that produced something.
func assertShapeIsHonest[T any](t *testing.T, command, payload string, items []T, identified func(T) bool) {
	t.Helper()

	if len(items) > 0 && !isJSONObject(payload) {
		t.Fatalf("%s: %q is not a JSON object yet decoded to %d item(s)", command, payload, len(items))
	}

	anyIdentified := false
	for _, item := range items {
		if identified(item) {
			anyIdentified = true
			break
		}
	}

	err := assertShape(command, items)
	switch {
	case len(items) == 0:
		if err != nil {
			t.Fatalf("%s: an empty item list was refused: %v", command, err)
		}
	case !anyIdentified:
		if err == nil {
			t.Fatalf("%s: %d item(s) decoded from %q, not one of which carries a field this build "+
				"understands, and the shape guard allowed it; the caller reports that as "+
				"\"no results\", which is what authorises a deletion", command, len(items), payload)
		}
	case err != nil:
		t.Fatalf("%s: %d item(s) including at least one this build understands were refused: %v",
			command, len(items), err)
	}
}

// isJSONObject reports whether the document is a JSON object rather than one of
// the other five documents that unmarshal into a struct without complaint.
func isJSONObject(payload string) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal([]byte(payload), &object) == nil && object != nil
}

// FuzzDevmapNoticesNeverSilentlyDropALine fuzzes the other half of a devmap
// answer — the free text on stderr, where the producer names what it left out.
//
// This half has no schema at all: it is prose from another repository, matched
// by two regular expressions. The failure it is written against is a classifier
// that stops matching and reports a clean build for one that refused half the
// tree, and the design answer was that anything unrecognised is carried out
// verbatim rather than dropped. That is a property, and this asserts it as one.
//
// The invariants:
//
//   - Classifying never panics, whatever the bytes.
//   - A truncated stream is always reported as truncated and never reads as
//     clean: what is missing from it is not knowable, so it cannot be counted.
//   - A report is clean exactly when it has nothing to describe.
//   - Appending a line the classifier does not recognise always changes the
//     report. This is the conservation property the type exists for, checked
//     differentially so it cannot be satisfied by reimplementing the parser
//     here and agreeing with it.
func FuzzDevmapNoticesNeverSilentlyDropALine(f *testing.F) {
	base := []string{
		"",
		"\n",
		"discovery refused 3 file(s)",
		"discovery refused 3 file(s)\n  a.go: Unreadable { reason: \"x\" }\n  b.go: TooLarge",
		"discovery refused 0 file(s)",
		"discovery refused 99999999999999999999 file(s)",
		"discovery refused 3 file(s)\n  … and 2 more",
		"discovery refused 3 file(s)\n  ... and 2 more",
		"discovery refused 2 file(s)\n  a.go: r\nnot indented\n  b.go: r",
		"warning: something else entirely",
		"  indented with no header",
		strings.Repeat("junk line\n", 200),
	}
	appended := []string{
		"",
		" ",
		"x",
		"warning: the index is stale",
		"a.go: Unreadable",
		"… and 4 more",
		"\x00",
		strings.Repeat("w", 4096),
	}
	for _, b := range base {
		for _, a := range appended {
			f.Add(b, a, false, defaultMaxStderr)
			f.Add(b, a, true, defaultMaxStderr)
		}
	}

	f.Fuzz(func(t *testing.T, text, extra string, truncated bool, limit int) {
		if !utf8.ValidString(text) || !utf8.ValidString(extra) {
			return
		}

		stream := said{text: []byte(text), truncated: truncated, limit: limit}
		notices := readNotices(stream)

		if notices.StreamTruncated != truncated {
			t.Fatalf("truncation was not carried into the report (got %v, want %v)",
				notices.StreamTruncated, truncated)
		}
		if truncated && notices.Clean() {
			t.Fatal("a report built from a truncated stderr reported itself clean; " +
				"an unknown number of refusals were never read")
		}
		// Clean and Degraded are the two faces of one answer, and a caller that
		// prints only the second while branching on the first would report a
		// degradation as nothing at all.
		if degraded := notices.Degraded(); notices.Clean() != (len(degraded) == 0) {
			t.Fatalf("Clean()=%v but Degraded() returned %d line(s): %v",
				notices.Clean(), len(degraded), degraded)
		}

		// Merging must conserve. A caller that runs build and manifest to
		// produce one artifact folds their reports, and a truncation or a count
		// lost in that fold is an unreported gap in the graph the write gate
		// reads.
		merged := notices
		merged.Merge(notices)
		if merged.Refused != notices.Refused*2 {
			t.Fatalf("merging a report with itself gave Refused=%d, want %d",
				merged.Refused, notices.Refused*2)
		}
		if notices.StreamTruncated && !merged.StreamTruncated {
			t.Fatal("merging dropped the truncation flag")
		}

		// The conservation property, differentially. A line the classifier does
		// not recognise must survive into the report verbatim, or — once the
		// retention cap is reached — be counted. The exclusions below are the
		// two shapes that are recognised by design and legitimately add nothing
		// on their own: a refusal header naming zero files, and the producer's
		// stand-in for the entries it did not print, which only matches beneath
		// a header it did not have here.
		line := strings.TrimSpace(extra)
		if line == "" || strings.Contains(line, "discovery refused") || refusalMore.MatchString(line) {
			return
		}
		// One line, because the claim is about one line. A multi-line value is
		// split by the classifier into several entries, none of which equals
		// the whole of it — the differential would then report a drop that did
		// not happen. Multi-line streams are covered by `text`, which is fuzzed
		// without restriction.
		if strings.ContainsAny(line, "\n\r") {
			return
		}
		after := readNotices(said{text: []byte(text + "\n" + line), truncated: truncated, limit: limit})
		if after.UnrecognisedHidden > notices.UnrecognisedHidden {
			return // past the retention cap, and counted rather than dropped
		}
		for _, kept := range after.Unrecognised {
			if kept == line {
				return
			}
		}
		t.Fatalf("appending the unrecognised line %q to %q changed nothing in the report "+
			"(Unrecognised=%d hidden=%d before, %d/%d after); a line this build cannot read "+
			"must be repeated, not dropped",
			line, text, len(notices.Unrecognised), notices.UnrecognisedHidden,
			len(after.Unrecognised), after.UnrecognisedHidden)
	})
}
