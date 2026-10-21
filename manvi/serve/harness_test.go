package serve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// None of these tests touches the task store: harness.resolve and the
// materialiser read and write a temporary git work tree and nothing else.

const goodSpec = `{"name":"team","sections":[{"name":"house-rules","order":70,"text":"Prefer small diffs."},` +
	`{"name":"closing","order":80,"text":"Say what you verified."}],` +
	`"roles":[{"name":"reviewer","description":"Reviews a diff","read_only":true}],"skills":["alpha"]}`

// testReserved are section names the fake prompt owns. In production the list
// comes from the command's assembler (see serveHarnessResolver, and its test in
// cmd/manvi); here it only has to contain an Essential name to collide with.
var testReserved = []string{"identity", "policy", "skills", "project-instructions"}

func newTestResolver(t *testing.T) *HarnessResolver {
	t.Helper()
	cat := &harness.Catalogue{}
	var named harness.Spec
	if err := json.Unmarshal([]byte(goodSpec), &named); err != nil {
		t.Fatal(err)
	}
	named.Name = "registered"
	if err := cat.Register(named); err != nil {
		t.Fatal(err)
	}
	return &HarnessResolver{
		Reserved:  func(string) []string { return testReserved },
		Catalogue: cat,
	}
}

func resolverServer(t *testing.T, r *HarnessResolver) Options {
	t.Helper()
	return Options{HardRules: true, Posture: PostureHost, Modules: []Module{HarnessModule{Resolver: r}}}
}

func resolveVia(t *testing.T, r *HarnessResolver, params any) Response {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return roundTrip(t, resolverServer(t, r), Request{ID: "1", Op: OpHarnessResolve, Params: raw})[0]
}

func decodeResolve(t *testing.T, resp Response) HarnessResolveResult {
	t.Helper()
	if !resp.OK {
		t.Fatalf("harness.resolve failed: %+v", resp.Error)
	}
	var out HarnessResolveResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func inlineParams(doc string) map[string]any {
	return map[string]any{
		"session_id": "tab-1", "provider": "anthropic",
		"spec": map[string]any{"kind": "inline", "inline": json.RawMessage(doc)},
	}
}

func TestHarnessResolveInlineReturnsSectionsAndTheDigestOfTheSpec(t *testing.T) {
	r := newTestResolver(t)
	got := decodeResolve(t, resolveVia(t, r, inlineParams(goodSpec)))

	spec, err := harness.DecodeSpec([]byte(goodSpec))
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != spec.Digest() {
		t.Fatalf("digest = %s, want the spec's own %s", got.Digest, spec.Digest())
	}
	if len(got.Sections) != 2 || got.Sections[0].Name != "house-rules" || got.Sections[1].Order != 80 {
		t.Fatalf("sections = %+v, want house-rules then closing, in digest order", got.Sections)
	}
	if len(got.Roles) != 1 || got.Roles[0].Name != "reviewer" || !got.Roles[0].ReadOnly {
		t.Fatalf("roles = %+v", got.Roles)
	}
	if len(got.Notes) == 0 || !strings.Contains(got.Notes[0], "no root") {
		t.Fatalf("an answer with no skills read must say why, got notes %v", got.Notes)
	}
}

func TestHarnessResolveDigestIsTheSameForTheSameSpecInAnotherKeyOrder(t *testing.T) {
	r := newTestResolver(t)
	reordered := `{"skills":["alpha"],"roles":[{"read_only":true,"description":"Reviews a diff","name":"reviewer"}],` +
		`"sections":[{"text":"Say what you verified.","order":80,"name":"closing"},{"text":"Prefer small diffs.","order":70,"name":"house-rules"}],"name":"team"}`
	a := decodeResolve(t, resolveVia(t, r, inlineParams(goodSpec)))
	b := decodeResolve(t, resolveVia(t, r, inlineParams(reordered)))
	if a.Digest != b.Digest {
		t.Fatalf("listing order changed the digest: %s vs %s", a.Digest, b.Digest)
	}
}

func TestHarnessResolveNamedReadsTheCatalogue(t *testing.T) {
	r := newTestResolver(t)
	got := decodeResolve(t, resolveVia(t, r, map[string]any{
		"session_id": "s", "provider": "local", "spec": map[string]any{"kind": "named", "name": "registered"},
	}))
	if len(got.Sections) != 2 || got.Digest == "" {
		t.Fatalf("named spec resolved to %+v", got)
	}
}

func TestHarnessResolveRepoReadsTheRepositorySpecAndFiltersSkills(t *testing.T) {
	root := scanRepo(t, map[string]string{
		".devcouncil/harness/team.json":     goodSpec,
		".agents/skills/alpha/SKILL.md":     "---\nname: alpha\ndescription: Does alpha things\n---\nbody never leaves the repo",
		".agents/skills/beta/SKILL.md":      "---\nname: beta\ndescription: Does beta things\n---\nbody",
		".agents/skills/broken/SKILL.md":    "no frontmatter at all",
		".devcouncil/harness/other.json":    `{"name":"other"}`,
		".devcouncil/harness/skills.json":   `{"name":"skills"}`,
		".devcouncil/harness/mismatch.json": `{"name":"someone-else"}`,
	})
	r := newTestResolver(t)
	got := decodeResolve(t, resolveVia(t, r, map[string]any{
		"session_id": "s", "provider": "anthropic", "root": root,
		"spec": map[string]any{"kind": "repo", "name": "team"},
	}))
	if len(got.Skills) != 1 || got.Skills[0].Name != "alpha" || got.Skills[0].Description != "Does alpha things" {
		t.Fatalf("skills = %+v, want only alpha (the spec selects it), as name and description", got.Skills)
	}
	if strings.Contains(string(mustJSON(t, got)), "body never leaves") {
		t.Fatal("a skill body reached the wire")
	}

	// Without a selection every skill is listed, and the one that was found and
	// refused is reported, never skipped.
	all := decodeResolve(t, resolveVia(t, r, map[string]any{
		"session_id": "s", "provider": "anthropic", "root": root,
		"spec": map[string]any{"kind": "repo", "name": "other"},
	}))
	names := []string{}
	for _, s := range all.Skills {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"alpha", "beta"}) {
		t.Fatalf("skills = %v, want alpha and beta", names)
	}
	if len(all.Rejected) != 1 || all.Rejected[0].Name != "broken" || all.Rejected[0].Reason == "" {
		t.Fatalf("rejected = %+v, want the malformed skill with its reason", all.Rejected)
	}

	// A spec file that holds a spec of another name is refused, not served.
	resp := resolveVia(t, r, map[string]any{
		"session_id": "s", "provider": "anthropic", "root": root,
		"spec": map[string]any{"kind": "repo", "name": "mismatch"},
	})
	if resp.OK || resp.Error.Code != ErrHarnessRefused {
		t.Fatalf("mismatched spec name: %+v", resp)
	}
}

func TestHarnessResolveSkillsListingIsBoundedByTheBudget(t *testing.T) {
	root := scanRepo(t, map[string]string{
		".devcouncil/harness/team.json": `{"name":"team"}`,
		".agents/skills/one/SKILL.md":   "---\nname: one\ndescription: " + strings.Repeat("word ", 40) + "\n---\nb",
		".agents/skills/two/SKILL.md":   "---\nname: two\ndescription: " + strings.Repeat("word ", 40) + "\n---\nb",
		".agents/skills/three/SKILL.md": "---\nname: three\ndescription: " + strings.Repeat("word ", 40) + "\n---\nb",
	})
	r := newTestResolver(t)
	r.Budget = func(string) HarnessBudget { return HarnessBudget{Skills: 70} }
	got := decodeResolve(t, resolveVia(t, r, map[string]any{
		"session_id": "s", "provider": "local", "root": root,
		"spec": map[string]any{"kind": "repo", "name": "team"},
	}))
	if len(got.Skills) == 0 || len(got.Skills) >= 3 {
		t.Fatalf("listed %d skills under a 70-token bound, want some but not all", len(got.Skills))
	}
	if !slices.ContainsFunc(got.Notes, func(n string) bool { return strings.Contains(n, "not listed") }) {
		t.Fatalf("a cut listing must say so, notes = %v", got.Notes)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A spec carries no authority: each field that would widen a session is refused
// by name, with the typed code, and nothing is resolved.
func TestHarnessResolveRefusesWhatASpecMayNotCarry(t *testing.T) {
	r := newTestResolver(t)
	cases := map[string]struct{ doc, want string }{
		"write_exceptions on a role": {
			`{"name":"x","roles":[{"name":"r","description":"d","write_exceptions":["src/**"]}]}`, "write_exceptions"},
		"write_exceptions at the top": {`{"name":"x","write_exceptions":["**"]}`, "write_exceptions"},
		"tier":                        {`{"name":"x","tier":"frontier"}`, "tier"},
		"posture":                     {`{"name":"x","posture":"yolo"}`, "posture"},
		"flags":                       {`{"name":"x","flags":{"policy.hard_rules":false}}`, "flags"},
		"mcp":                         {`{"name":"x","mcp":{"servers":[]}}`, "mcp"},
		"verify":                      {`{"name":"x","verify":"true"}`, "verify"},
		"fetch":                       {`{"name":"x","fetch":["example.com"]}`, "fetch"},
		"mixed case key":              {`{"name":"x","Tier":"t"}`, "Tier"},
		"unknown key":                 {`{"name":"x","telemetry":true}`, "telemetry"},
		"duplicate key":               {`{"name":"x","name":"y"}`, "duplicate"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resp := resolveVia(t, r, inlineParams(c.doc))
			if resp.OK {
				t.Fatalf("a spec with %s was resolved: %s", name, resp.Result)
			}
			if resp.Error.Code != ErrHarnessRefused {
				t.Fatalf("code = %s, want %s (%s)", resp.Error.Code, ErrHarnessRefused, resp.Error.Message)
			}
			if !strings.Contains(strings.ToLower(resp.Error.Message), strings.ToLower(c.want)) {
				t.Fatalf("the refusal does not name %q: %s", c.want, resp.Error.Message)
			}
		})
	}
}

func TestHarnessResolveRefusesAnEssentialOrReservedSectionName(t *testing.T) {
	r := newTestResolver(t)
	for _, name := range testReserved {
		doc := fmt.Sprintf(`{"name":"x","sections":[{"name":%q,"order":70,"text":"t"}]}`, name)
		resp := resolveVia(t, r, inlineParams(doc))
		if resp.OK || resp.Error.Code != ErrHarnessRefused || !strings.Contains(resp.Error.Message, name) {
			t.Fatalf("section %q: %+v", name, resp)
		}
	}
}

func TestHarnessResolveListsEveryProblemNotTheFirst(t *testing.T) {
	r := newTestResolver(t)
	resp := resolveVia(t, r, inlineParams(
		`{"name":"x","sections":[{"name":"policy","order":70,"text":"a"},{"name":"b","order":5,"text":"b"},{"name":"c","order":70,"text":""}]}`))
	if resp.OK {
		t.Fatal("resolved")
	}
	for _, want := range []string{"sections[0]", "sections[1]", "sections[2]", "3 problem"} {
		if !strings.Contains(resp.Error.Message, want) {
			t.Fatalf("the refusal lacks %q: %s", want, resp.Error.Message)
		}
	}
}

func TestHarnessResolveRefusesAMalformedRequest(t *testing.T) {
	r := newTestResolver(t)
	cases := map[string]any{
		"no session":        map[string]any{"provider": "local", "spec": map[string]any{"kind": "named", "name": "registered"}},
		"no provider":       map[string]any{"session_id": "s", "spec": map[string]any{"kind": "named", "name": "registered"}},
		"unknown kind":      map[string]any{"session_id": "s", "provider": "local", "spec": map[string]any{"kind": "remote", "name": "x"}},
		"repo with no root": map[string]any{"session_id": "s", "provider": "local", "spec": map[string]any{"kind": "repo", "name": "x"}},
		"inline missing":    map[string]any{"session_id": "s", "provider": "local", "spec": map[string]any{"kind": "inline"}},
		"inline and name":   map[string]any{"session_id": "s", "provider": "local", "spec": map[string]any{"kind": "inline", "name": "other", "inline": json.RawMessage(goodSpec)}},
		"inline on named":   map[string]any{"session_id": "s", "provider": "local", "spec": map[string]any{"kind": "named", "name": "registered", "inline": json.RawMessage(goodSpec)}},
		// A field this build does not know could be a widening the host believes
		// was applied, so it is refused, not dropped.
		"unknown param":    map[string]any{"session_id": "s", "provider": "local", "tier": "frontier", "spec": map[string]any{"kind": "named", "name": "registered"}},
		"unknown spec key": map[string]any{"session_id": "s", "provider": "local", "spec": map[string]any{"kind": "named", "name": "registered", "write_exceptions": []string{"**"}}},
		"bad provider":     map[string]any{"session_id": "s", "provider": "../x", "spec": map[string]any{"kind": "named", "name": "registered"}},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			resp := resolveVia(t, r, params)
			if resp.OK {
				t.Fatalf("resolved: %s", resp.Result)
			}
			if resp.Error.Code != ErrBadRequest && resp.Error.Code != ErrBadRoot {
				t.Fatalf("code = %s, want a bad request: %s", resp.Error.Code, resp.Error.Message)
			}
		})
	}
}

func TestHarnessResolveReportsAbsenceDistinctlyFromRefusal(t *testing.T) {
	root := scanRepo(t, map[string]string{"README.md": "x"})
	r := newTestResolver(t)
	for name, spec := range map[string]map[string]any{
		"named": {"kind": "named", "name": "nonesuch"},
		"repo":  {"kind": "repo", "name": "nonesuch"},
	} {
		resp := resolveVia(t, r, map[string]any{"session_id": "s", "provider": "local", "root": root, "spec": spec})
		if resp.OK || resp.Error.Code != ErrHarnessNotFound {
			t.Fatalf("%s: %+v, want %s", name, resp, ErrHarnessNotFound)
		}
	}
}

func TestHarnessResolveCannotReadARepositorySpecTheReadGateWithholds(t *testing.T) {
	// A spec named like a secret is not a spec file: the one reader judges the
	// path as read_file would, so the gate cannot be skirted through this op.
	root := scanRepo(t, map[string]string{"README.md": "x"})
	if err := os.MkdirAll(filepath.Join(root, harness.SpecsDir), 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "real.json")
	if err := os.WriteFile(target, []byte(goodSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, harness.SpecsDir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	resp := resolveVia(t, newTestResolver(t), map[string]any{
		"session_id": "s", "provider": "local", "root": root, "spec": map[string]any{"kind": "repo", "name": "linked"},
	})
	if resp.OK {
		t.Fatalf("a spec reached through a symbolic link was served: %s", resp.Result)
	}
}

func TestHarnessModuleRefusesToConfigureWithoutTheReservedNameSource(t *testing.T) {
	for name, m := range map[string]HarnessModule{
		"nil resolver":   {},
		"no reserved":    {Resolver: &HarnessResolver{}},
		"typed nil only": {Resolver: (*HarnessResolver)(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(&strings.Builder{}, Options{Modules: []Module{m}})
			if s.configErr == nil {
				t.Fatal("a server was built whose harness op could not tell a reserved name from a free one")
			}
		})
	}
}

// hello is how a host learns an optional op exists; a build without the module
// does not list it, and a host that does not see it runs without a harness.
func TestHelloAdvertisesHarnessResolveOnlyWhenConfigured(t *testing.T) {
	if got := opsOf(t, Options{}); slices.Contains(got, OpHarnessResolve) {
		t.Fatalf("a build with no harness module advertises %s: %v", OpHarnessResolve, got)
	}
	if ops := opsOf(t, resolverServer(t, newTestResolver(t))); !slices.Contains(ops, OpHarnessResolve) {
		t.Fatalf("a build with the harness module does not advertise %s: %v", OpHarnessResolve, ops)
	}
	// And the degraded path is a typed refusal, not a hang or a guess.
	resp := roundTrip(t, Options{}, Request{ID: "1", Op: OpHarnessResolve, Params: json.RawMessage(`{}`)})[0]
	if resp.OK || resp.Error.Code != ErrBadRequest {
		t.Fatalf("an unserved op answered %+v", resp)
	}
}

// The harness changing between prepares is a prefix move, noted once; the
// ledger keeps every shortened result as it was and none is shortened again.
func TestPrepareNotesAHarnessChangeAsOnePrefixMoveWithoutReShortening(t *testing.T) {
	history := bulkyHistory(10, 60)
	params := PrepareParams{SessionID: "tab", System: "base", Messages: history, ContextWindow: 8192, HarnessDigest: "aaaa1111"}
	d := newDriver(t)

	first := d.callPrepare(params)
	if len(first.Steps) == 0 {
		t.Fatal("nothing was compacted; the fixture is not over budget")
	}
	if first.PrefixMoved || first.PrefixNote != "" {
		t.Fatalf("a session's first digest is a statement, not a move: %+v", first)
	}
	shortened := map[string]string{}
	for _, s := range first.Steps {
		shortened[s.ToolCallID] = s.Text
	}
	for i := range history {
		if text, ok := shortened[history[i].ToolCallID]; ok && history[i].Role == "tool" {
			history[i].Text = text
		}
	}
	params.Messages = history

	same := d.callPrepare(params)
	if same.PrefixMoved || len(same.Steps) != 0 {
		t.Fatalf("the same digest and history moved the prefix or re-planned: %+v", same)
	}

	params.HarnessDigest = "bbbb2222"
	params.System = "base plus a different harness section"
	moved := d.callPrepare(params)
	if !moved.PrefixMoved || !strings.Contains(moved.PrefixNote, "aaaa1111") || !strings.Contains(moved.PrefixNote, "bbbb2222") {
		t.Fatalf("a changed digest was not noted as a prefix move: %+v", moved)
	}
	for _, s := range moved.Steps {
		if _, seen := shortened[s.ToolCallID]; seen {
			t.Fatalf("%s was shortened again after the harness changed", s.ToolCallID)
		}
	}

	again := d.callPrepare(params)
	if again.PrefixMoved || again.PrefixNote != "" {
		t.Fatalf("one change was noted twice: %+v", again)
	}
	session := d.srv.chat.get("tab")
	if session.prefixMoves != 1 {
		t.Fatalf("prefix moves = %d, want exactly 1", session.prefixMoves)
	}
	for id, text := range shortened {
		if session.compacted[llm.CallID(id)] != text {
			t.Fatalf("the ledger entry for %s changed across the harness change", id)
		}
	}
}

// A prepare that does not state a digest says nothing about the harness: it is
// never read as the harness having been removed.
func TestPrepareWithoutADigestIsNotAHarnessChange(t *testing.T) {
	d := newDriver(t)
	params := PrepareParams{SessionID: "t", System: "s", Messages: []WireMessage{{Role: "user", Text: "hi"}}, ContextWindow: 8192, HarnessDigest: "aaaa"}
	d.callPrepare(params)
	params.HarnessDigest = ""
	if got := d.callPrepare(params); got.PrefixMoved {
		t.Fatalf("omitting the digest moved the prefix: %+v", got)
	}
	params.HarnessDigest = "aaaa"
	if got := d.callPrepare(params); got.PrefixMoved {
		t.Fatalf("restating the same digest moved the prefix: %+v", got)
	}
}

func TestPrepareRefusesAnOversizedHarnessDigest(t *testing.T) {
	d := newDriver(t)
	resp := d.call(prepareRequest(t, "1", PrepareParams{
		SessionID: "t", Messages: []WireMessage{{Role: "user", Text: "hi"}}, ContextWindow: 8192,
		HarnessDigest: strings.Repeat("a", maxHarnessDigestLen+1),
	}))
	if resp.OK || resp.Error.Code != ErrBadRequest {
		t.Fatalf("an oversized digest was accepted: %+v", resp)
	}
}
