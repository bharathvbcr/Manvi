package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write lays out a synthetic module so the checker can be exercised against
// code whose answer is known.
//
// The checker is the thing standing between this repo and a defect class that
// already shipped 23 times, which makes a checker that quietly stops working
// worse than no checker: a green run would then mean "nothing was examined"
// while reading exactly like "nothing was wrong". Fixtures with a known answer
// are what keeps its silence meaningful.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func names(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Name)
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

const catalogSrc = `package flags

const (
	Wired   = "a.wired"
	Inert   = "a.inert"
	Self    = "a.self"
)

type Def struct{ Key string }

func All() []Def {
	return []Def{
		{Key: Wired},
		{Key: Inert},
		{Key: Self},
	}
}

// EffectiveSelf reads Self inside the catalogue itself, which is real wiring:
// callers reach the setting through this helper.
func EffectiveSelf(r *Registry) bool { return r.Bool(Self) }

type Registry struct{}

func (r *Registry) Bool(k string) bool { return k != "" }
`

func TestFlagsWithoutReadersFindsOnlyTheInertOne(t *testing.T) {
	root := write(t, map[string]string{
		"flags/catalog.go": catalogSrc,
		"cmd/app.go": `package cmd

import "m/flags"

func run(r *flags.Registry) bool { return r.Bool(flags.Wired) }
`,
	})
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	got := names(m.FlagsWithoutReaders("flags/catalog.go", nil))

	if !has(got, "Inert") {
		t.Errorf("the inert flag was not reported: %v", got)
	}
	if has(got, "Wired") {
		t.Errorf("a flag read by another package was reported as inert: %v", got)
	}
	// The regression that motivated this: excluding the whole catalogue file
	// as a reader reported a flag resolved by a helper beside its own Def.
	if has(got, "Self") {
		t.Errorf("a flag resolved by a helper in the catalogue was reported as inert: %v", got)
	}
}

// A checker that cannot find the catalogue must fail, not report a clean sweep.
func TestAMissingCatalogueIsAFindingNotAPass(t *testing.T) {
	root := write(t, map[string]string{"cmd/app.go": "package cmd\n"})
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.FlagsWithoutReaders("flags/nope.go", nil); len(got) == 0 {
		t.Fatal("a missing catalogue produced no finding; a check that could not run reported a pass")
	}
}

const defSrc = `package agents

type Definition struct {
	Read   string ` + "`json:\"read\"`" + `
	Unread string ` + "`json:\"unread\"`" + `
	Padded string ` + "`json:\"padded\"`" + `
	noTag  string
}

// Register defaults Padded without giving it meaning. Self-defaulting is
// exactly what made these survive review, so it must not count as a reader.
func Register(d Definition) Definition {
	if d.Padded == "" {
		d.Padded = "inherit"
	}
	return d
}
`

func TestFieldsWithoutReadersIgnoresSelfDefaulting(t *testing.T) {
	root := write(t, map[string]string{
		"agents/definition.go": defSrc,
		"cmd/use.go": `package cmd

import "m/agents"

func use(d agents.Definition) string { return d.Read }
`,
	})
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	got := names(m.FieldsWithoutReaders("Definition", "agents/definition.go", nil))

	for _, want := range []string{"Definition.Unread", "Definition.Padded"} {
		if !has(got, want) {
			t.Errorf("%s was not reported: %v", want, got)
		}
	}
	if has(got, "Definition.Read") {
		t.Errorf("a field read elsewhere was reported as inert: %v", got)
	}
}

const handlerSrc = `package devcouncil

func decode(call Call, v any) error { return nil }

type Call struct{}
type Result struct{}

func handler(call Call) Result {
	var args struct {
		Used    string ` + "`json:\"used\"`" + `
		Dropped string ` + "`json:\"dropped\"`" + `
		Refused string ` + "`json:\"refused\"`" + `
	}
	if err := decode(call, &args); err != nil {
		return Result{}
	}
	_ = args.Used
	if args.Refused != "" {
		return Result{}
	}
	return Result{}
}

func shaped(call Call) Result {
	// A named result type on its way to JSON. Its fields are written, never
	// read, and reporting them would bury the real finding.
	type row struct {
		Path string ` + "`json:\"path\"`" + `
		Line int    ` + "`json:\"line\"`" + `
	}
	rows := []row{{Path: "p", Line: 1}}
	_ = rows
	return Result{}
}
`

func TestArgFieldsWithoutReadersSkipsResultStructs(t *testing.T) {
	root := write(t, map[string]string{"devcouncil/h.go": handlerSrc})
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	got := names(m.ArgFieldsWithoutReaders(nil))

	if !has(got, "handler.Dropped") {
		t.Errorf("a decoded-and-dropped argument was not reported: %v", got)
	}
	if has(got, "handler.Used") {
		t.Errorf("a read argument was reported: %v", got)
	}
	// Refusing a retired parameter by name is a legitimate, and better,
	// alternative to honouring it — it must count as read.
	if has(got, "handler.Refused") {
		t.Errorf("an argument read in order to be refused was reported: %v", got)
	}
	for _, never := range []string{"shaped.Path", "shaped.Line"} {
		if has(got, never) {
			t.Errorf("a result-struct field was reported as an unread argument: %v", got)
		}
	}
}

// An allowlist must excuse only what it names.
func TestAllowlistExcusesOnlyWhatItNames(t *testing.T) {
	root := write(t, map[string]string{"flags/catalog.go": catalogSrc})
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	got := names(m.FlagsWithoutReaders("flags/catalog.go", map[string]string{
		"Inert": "excused for this test",
	}))
	if has(got, "Inert") {
		t.Errorf("an allowlisted flag was still reported: %v", got)
	}
}

// Load must refuse an empty tree rather than report a clean sweep over nothing.
func TestLoadRefusesATreeWithNoSource(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load accepted a tree with no Go files; every check would then pass vacuously")
	}
}

// Test files must never count as readers: a flag exercised only by its own unit
// test still changes nothing about a run.
func TestTestFilesDoNotCountAsReaders(t *testing.T) {
	root := write(t, map[string]string{
		"flags/catalog.go": catalogSrc,
		"cmd/app_test.go": `package cmd

import "m/flags"

func use() string { return flags.Inert }
`,
	})
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(m.FlagsWithoutReaders("flags/catalog.go", nil)); !has(got, "Inert") {
		t.Errorf("a flag read only by a test was treated as wired: %v", got)
	}
}

func TestFindingStringNamesTheThingAndThePlace(t *testing.T) {
	f := Finding{Kind: "flag", Name: "X", Where: "a/b.go:12", Why: "because"}
	s := f.String()
	for _, want := range []string{"flag", "X", "a/b.go:12", "because"} {
		if !strings.Contains(s, want) {
			t.Errorf("Finding.String() = %q, missing %q", s, want)
		}
	}
}

const schemaSrc = `package devcouncil

func schema(name, desc, s string) any { return nil }

func decode(c Call, v any) error { return nil }

type Call struct{}

var tools = []any{
	schema("t_good", "d", ` + "`" + `{"type":"object","properties":{"used":{"type":"string","description":"d"},"phantom":{"type":"boolean"}},"required":["used"]}` + "`" + `),
}

func handler(call Call) {
	var args struct {
		Used string ` + "`json:\"used\"`" + `
	}
	_ = decode(call, &args)
	_ = args.Used
}
`

func TestSchemaPropsWithoutDecodersFindsTheUndecodedOne(t *testing.T) {
	root := write(t, map[string]string{"devcouncil/t.go": schemaSrc})
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	got := names(m.SchemaPropsWithoutDecoders(nil))

	if !has(got, "t_good.phantom") {
		t.Errorf("a schema property nothing decodes was not reported: %v", got)
	}
	if has(got, "t_good.used") {
		t.Errorf("a decoded property was reported: %v", got)
	}
	// Schema keywords are not properties.
	for _, keyword := range []string{"t_good.type", "t_good.description", "t_good.required"} {
		if has(got, keyword) {
			t.Errorf("a JSON Schema keyword was reported as a property: %v", got)
		}
	}
}

func TestSchemaPropertyExtraction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		want   []string
	}{
		{
			name:   "flat",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]}`,
			want:   []string{"a", "b"},
		},
		{
			name: "nested array of objects",
			schema: `{"type":"object","properties":{"tasks":{"type":"array","items":{"type":"object",` +
				`"properties":{"label":{"type":"string"},"prompt":{"type":"string"}},"required":["label"]}}},"required":["tasks"]}`,
			want: []string{"tasks", "label", "prompt"},
		},
		{
			name:   "empty",
			schema: `{"type":"object","properties":{}}`,
			want:   nil,
		},
		{
			name:   "no properties at all",
			schema: `{"type":"object"}`,
			want:   nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := schemaProperties(tc.schema)
			if len(got) != len(tc.want) {
				t.Fatalf("schemaProperties(%s) = %v, want %v", tc.schema, got, tc.want)
			}
			for _, w := range tc.want {
				if !has(got, w) {
					t.Errorf("missing property %q in %v", w, got)
				}
			}
		})
	}
}

// Malformed or truncated schemas must not hang or panic the checker.
func TestSchemaPropertyExtractionSurvivesMalformedInput(t *testing.T) {
	for _, bad := range []string{
		``, `{`, `{"properties"`, `{"properties":`, `{"properties":{`,
		`{"properties":{"a"`, `{"properties":{"a":`, `{"properties":{"a":{`,
		`"properties"`, `{"properties":{"a":{"b":{"c":{`,
	} {
		done := make(chan []string, 1)
		go func() { done <- schemaProperties(bad) }()
		select {
		case <-done:
		case <-timeAfter():
			t.Fatalf("schemaProperties(%q) did not terminate", bad)
		}
	}
}

// timeAfter bounds the malformed-input test so a scanner that fails to advance
// shows up as a failure rather than a hung suite.
func timeAfter() <-chan time.Time { return time.After(5 * time.Second) }
