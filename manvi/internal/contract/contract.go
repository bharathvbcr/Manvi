// Package contract is the static check that keeps a declared capability from
// being an inert one.
//
// The harness declares what it can do in three places a reader trusts: the flag
// catalogue, the JSON schemas it shows a model, and the struct fields that
// describe a sub-agent role. Every one of those is a promise, and none of them
// is enforced by the compiler — an unread flag key compiles, a schema property
// nothing decodes compiles, and a struct field assigned and never consulted
// compiles. So the promise and the behaviour drift, silently, and the drift is
// invisible in review because the declaration reads correctly.
//
// That is not hypothetical. A single audit of this repository found twelve
// declared capabilities with no implementation behind them: the guidance
// router and its ActiveGroups condition, the QuestionAsker seam, the
// pair.questions.enabled flag, six fields of agents.Definition, and two tool
// schema properties that were decoded into fields nothing read. Each shipped,
// each looked deliberate, and each told a model or an operator it had control
// it did not have. The tool schemas are the sharpest case: a property advertised
// and dropped is the harness lying to the model about its own interface.
//
// Every one of those was found by grepping for readers. This package is that
// grep, made total and made a test — so the class fails at commit rather than
// being rediscovered by the next audit.
package contract

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Finding is one declared capability with nothing behind it.
type Finding struct {
	// Kind names the declaration layer: "flag", "role-field", "tool-arg".
	Kind string
	// Name is the declared thing — a flag constant, a field, a schema property.
	Name string
	// Where is the declaration site, as file:line.
	Where string
	// Why explains what the absence means for a caller who trusted it.
	Why string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s %s (%s): %s", f.Kind, f.Name, f.Where, f.Why)
}

// Module is a parsed view of the harness source.
type Module struct {
	root  string
	fset  *token.FileSet
	files map[string]*ast.File // path -> AST, production files only
}

// Load parses every non-test Go file under root.
//
// Test files are excluded deliberately, and that exclusion is the point of the
// check rather than a shortcut. A flag read only by its own unit test is still
// a flag that changes nothing about a run; counting that test as a reader would
// make the check pass for exactly the code it exists to catch.
func Load(root string) (*Module, error) {
	m := &Module{root: root, fset: token.NewFileSet(), files: map[string]*ast.File{}}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "vendor", ".git", "target":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(m.fset, path, nil, parser.ParseComments)
		if perr != nil {
			return fmt.Errorf("parsing %s: %w", path, perr)
		}
		m.files[path] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(m.files) == 0 {
		return nil, fmt.Errorf("contract: no Go files found under %s", root)
	}
	return m, nil
}

func (m *Module) pos(n ast.Node) string {
	p := m.fset.Position(n.Pos())
	rel, err := filepath.Rel(m.root, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	return fmt.Sprintf("%s:%d", rel, p.Line)
}

// selectorUses counts uses of `.name` — a field read or a method call — across
// the module, skipping the files named in except.
//
// Matching on the selector alone rather than on a resolved type is deliberate.
// Full type resolution would need the whole package graph loaded and would make
// this check something a contributor cannot run quickly, and the failure mode
// of the loose match is a false negative on a same-named field elsewhere, never
// a false positive. A check that cries wolf gets deleted; one that occasionally
// misses still catches the class.
func (m *Module) selectorUses(name string, except map[string]bool) int {
	count := 0
	for path, file := range m.files {
		if except[path] {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				count++
			}
			return true
		})
	}
	return count
}

// FlagsWithoutReaders reports flag key constants that nothing outside the flags
// package consults.
//
// A flag is a promise to an operator that a setting changes a run. One that no
// production code reads changes nothing, reports its default through
// `manvi flags` as though it were in force, and is indistinguishable from a
// working setting until someone tries to rely on it.
func (m *Module) FlagsWithoutReaders(catalogFile string, allow map[string]string) []Finding {
	catalogPath := filepath.Join(m.root, catalogFile)
	file, ok := m.files[catalogPath]
	if !ok {
		return []Finding{{
			Kind: "flag", Name: catalogFile, Where: catalogFile,
			Why: "the flag catalogue was not found, so no flag could be checked; " +
				"a check that cannot run must not pass",
		}}
	}

	// The keys that matter are the ones the catalogue actually defines, which
	// is the set named in a `Key:` field of a Def literal. A constant that is
	// declared but never put in the catalogue is a different problem and not
	// this one.
	defined := map[string]ast.Node{}
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Key" {
			return true
		}
		if id, ok := kv.Value.(*ast.Ident); ok {
			defined[id.Name] = kv
		}
		return true
	})

	// The const declaration and the `Key:` entry are the two places a flag
	// names itself; neither is a reader. Everything else counts, including a
	// resolver living in the catalogue file — flags.EffectiveHardRules reads
	// policy.hard_rules.enabled a few hundred lines below its own Def, and
	// that is genuine wiring, not a self-reference. Excluding the whole file
	// would have reported it as dead.
	skip := declarationSites(file, defined)

	var out []Finding
	for name, node := range defined {
		if _, excused := allow[name]; excused {
			continue
		}
		if m.identUsesExcept(name, skip) > 0 {
			continue
		}
		out = append(out, Finding{
			Kind: "flag", Name: name, Where: m.pos(node),
			Why: "defined in the catalogue and read by no production code, so setting it changes nothing; " +
				"wire it to the behaviour it names, or remove it",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FieldsWithoutReaders reports exported, JSON-tagged fields of a named struct
// that nothing reads outside the file declaring it.
//
// This is the agents.Definition case. A role that says enable_mcp_tools:false
// and still receives the MCP tools has not been misconfigured — it has been
// lied to, and the operator who wrote the line has no way to discover it.
func (m *Module) FieldsWithoutReaders(structName, declFile string, allow map[string]string) []Finding {
	declPath := filepath.Join(m.root, declFile)
	file, ok := m.files[declPath]
	if !ok {
		return []Finding{{
			Kind: "role-field", Name: structName, Where: declFile,
			Why: "the declaring file was not found, so no field could be checked; " +
				"a check that cannot run must not pass",
		}}
	}

	// The declaring file is excused as a reader: a struct's own defaulting
	// ("if f.Model == \"\" { f.Model = \"inherit\" }") reads the field without
	// making it mean anything, and treating that as wiring is precisely how
	// every one of these survived review.
	except := map[string]bool{declPath: true}

	var out []Finding
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != structName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			if field.Tag == nil || !strings.Contains(field.Tag.Value, "json:") {
				continue
			}
			for _, fname := range field.Names {
				if !fname.IsExported() {
					continue
				}
				if _, excused := allow[fname.Name]; excused {
					continue
				}
				if m.selectorUses(fname.Name, except) > 0 {
					continue
				}
				out = append(out, Finding{
					Kind: "role-field", Name: structName + "." + fname.Name, Where: m.pos(fname),
					Why: "carried in the type and read nowhere outside its own declaration, so setting it " +
						"has no effect; make it load-bearing, or remove it and refuse the retired key",
				})
			}
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ArgFieldsWithoutReaders reports fields on anonymous argument structs — the
// `var args struct{...}` a tool handler decodes into — that the handler never
// reads.
//
// This is the sharpest of the three, because the argument struct mirrors a JSON
// schema the model is shown. A field decoded and dropped means the schema
// advertises a parameter that does nothing: the model sets it, the call
// succeeds, and the setting is discarded without a word. Two of these shipped
// here — a per-call model and a per-call workspace on the sub-agent tools.
//
// A field read only to be refused still counts as read, which is the correct
// reading: refusing a retired parameter loudly is a legitimate, and better,
// alternative to honouring it.
func (m *Module) ArgFieldsWithoutReaders(allow map[string]string) []Finding {
	var out []Finding
	for path, file := range m.files {
		ast.Inspect(file, func(n ast.Node) bool {
			decl, ok := n.(*ast.FuncDecl)
			if !ok || decl.Body == nil {
				return true
			}
			// Collect the names this function reads as `x.Field` anywhere in
			// its own body, then compare against what it declared.
			read := map[string]bool{}
			ast.Inspect(decl.Body, func(inner ast.Node) bool {
				if sel, ok := inner.(*ast.SelectorExpr); ok {
					read[sel.Sel.Name] = true
				}
				return true
			})

			// Only the anonymous `var args struct{...}` a handler decodes
			// counts. A named result type declared in the same body — `type
			// hit struct{...}` on its way to JSON — has fields that are
			// written and never read by construction, and reporting those
			// would bury the real finding under a dozen that are correct.
			decoded := decodedArgStructs(decl.Body)
			for _, st := range decoded {
				for _, field := range st.Fields.List {
					if field.Tag == nil || !strings.Contains(field.Tag.Value, "json:") {
						continue
					}
					for _, fname := range field.Names {
						key := decl.Name.Name + "." + fname.Name
						if _, excused := allow[key]; excused {
							continue
						}
						if read[fname.Name] {
							continue
						}
						rel, _ := filepath.Rel(m.root, path)
						_ = rel
						out = append(out, Finding{
							Kind: "tool-arg", Name: key, Where: m.pos(fname),
							Why: "decoded from the model's arguments and never read, so the schema advertises a " +
								"parameter that does nothing; honour it, or refuse it by name",
						})
					}
				}
			}
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Where < out[j].Where
	})
	return out
}

// declarationSites marks the positions where a flag names itself: the const
// spec and the `Key:` entry in its Def. Neither is a reader.
func declarationSites(file *ast.File, defined map[string]ast.Node) map[token.Pos]bool {
	skip := map[token.Pos]bool{}
	for _, node := range defined {
		if kv, ok := node.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Value.(*ast.Ident); ok {
				skip[id.Pos()] = true
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for _, name := range vs.Names {
			if _, isFlag := defined[name.Name]; isFlag {
				skip[name.Pos()] = true
			}
		}
		return true
	})
	return skip
}

// identUsesExcept counts uses of an identifier across the module, ignoring the
// exact positions given.
func (m *Module) identUsesExcept(name string, skip map[token.Pos]bool) int {
	count := 0
	for _, file := range m.files {
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name && !skip[id.Pos()] {
				count++
			}
			return true
		})
	}
	return count
}

// decodedArgStructs returns the anonymous struct types declared in a `var` and
// then handed to decode — the handler's view of the model's arguments.
func decodedArgStructs(body *ast.BlockStmt) []*ast.StructType {
	// Names passed to decode(call, &x) or json.Unmarshal(..., &x).
	decoded := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := ""
		switch f := call.Fun.(type) {
		case *ast.Ident:
			fn = f.Name
		case *ast.SelectorExpr:
			fn = f.Sel.Name
		}
		if fn != "decode" && fn != "Unmarshal" {
			return true
		}
		for _, arg := range call.Args {
			unary, ok := arg.(*ast.UnaryExpr)
			if !ok || unary.Op != token.AND {
				continue
			}
			if id, ok := unary.X.(*ast.Ident); ok {
				decoded[id.Name] = true
			}
		}
		return true
	})

	var out []*ast.StructType
	ast.Inspect(body, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		st, ok := vs.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, name := range vs.Names {
			if decoded[name.Name] {
				out = append(out, st)
				break
			}
		}
		return true
	})
	return out
}

// SchemaPropsWithoutDecoders reports JSON schema properties a tool advertises
// to the model that nothing in the package ever decodes.
//
// This is the other half of ArgFieldsWithoutReaders, and the two fail in
// opposite directions. That one catches a parameter decoded and dropped; this
// one catches a parameter never decoded at all — advertised in the schema the
// model is shown, absent from every argument struct, so setting it is silently
// void. Both are the harness lying to the model about its own interface, and a
// model cannot audit either: the call succeeds both times.
//
// The match is by json tag name across the whole module rather than per
// handler, because a schema and the struct that decodes it are often written a
// few hundred lines apart and sometimes in different files. That makes the
// check conservative — it answers "does anything anywhere decode this name" —
// which is the right bias for a rule that must not cry wolf.
func (m *Module) SchemaPropsWithoutDecoders(allow map[string]string) []Finding {
	decoded := m.jsonTagNames()

	var out []Finding
	for path, file := range m.files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "schema" || len(call.Args) < 3 {
				return true
			}
			lit, ok := call.Args[len(call.Args)-1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			toolName := literalText(call.Args[0])
			for _, prop := range schemaProperties(literalText(lit)) {
				key := toolName + "." + prop
				if _, excused := allow[key]; excused {
					continue
				}
				if decoded[prop] {
					continue
				}
				rel, _ := filepath.Rel(m.root, path)
				_ = rel
				out = append(out, Finding{
					Kind: "schema-prop", Name: key, Where: m.pos(lit),
					Why: "advertised to the model in this tool's schema and decoded by no argument struct, " +
						"so setting it is silently void; decode and honour it, or drop it from the schema " +
						"and refuse the retired key",
				})
			}
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// jsonTagNames collects every name that appears in a `json:"name"` struct tag
// anywhere in the module — the set of things some struct can decode.
func (m *Module) jsonTagNames() map[string]bool {
	out := map[string]bool{}
	for _, file := range m.files {
		ast.Inspect(file, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				if name := jsonTagName(field.Tag.Value); name != "" {
					out[name] = true
				}
			}
			return true
		})
	}
	return out
}

// jsonTagName pulls the name out of a raw struct tag literal.
func jsonTagName(raw string) string {
	i := strings.Index(raw, `json:"`)
	if i < 0 {
		return ""
	}
	rest := raw[i+len(`json:"`):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	name := rest[:end]
	if comma := strings.IndexByte(name, ','); comma >= 0 {
		name = name[:comma]
	}
	if name == "-" {
		return ""
	}
	return name
}

// literalText strips the quoting from a Go string literal node.
func literalText(n ast.Node) string {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s := lit.Value
	if len(s) >= 2 {
		s = s[1 : len(s)-1]
	}
	return s
}

// schemaProperties pulls the property names out of a JSON Schema written as a
// Go string literal.
//
// It reads the literal rather than unmarshalling it because these schemas are
// assembled with string concatenation in places, so a parse would fail on
// exactly the schemas most worth checking. Names nested under a "properties"
// object are what a caller sets, so those are what is collected — including
// nested ones, since an array-of-objects parameter advertises its own fields
// and those go undecoded just as easily.
func schemaProperties(schema string) []string {
	var out []string
	seen := map[string]bool{}
	rest := schema
	for {
		i := strings.Index(rest, `"properties"`)
		if i < 0 {
			return out
		}
		rest = rest[i+len(`"properties"`):]
		// Walk the object that follows, collecting keys at its top level.
		depth := 0
		j := 0
		for ; j < len(rest); j++ {
			switch rest[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					j = len(rest)
				}
			case '"':
				if depth != 1 {
					continue
				}
				end := strings.IndexByte(rest[j+1:], '"')
				if end < 0 {
					return out
				}
				name := rest[j+1 : j+1+end]
				// A key at depth 1 is a property name; skip the schema
				// keywords that can appear beside them.
				if name != "" && !seen[name] && !isSchemaKeyword(name) {
					seen[name] = true
					out = append(out, name)
				}
				j += end + 1
				// Skip past this key's value so its inner keys are not read
				// as siblings.
				colon := strings.IndexByte(rest[j:], ':')
				if colon < 0 {
					return out
				}
				j += colon
				k, d := j+1, 0
				for ; k < len(rest); k++ {
					if rest[k] == '{' {
						d++
					} else if rest[k] == '}' {
						d--
						if d == 0 {
							break
						}
					} else if d == 0 && rest[k] == ',' {
						break
					}
				}
				j = k
			}
			if j >= len(rest) {
				break
			}
		}
	}
}

func isSchemaKeyword(name string) bool {
	switch name {
	case "type", "description", "properties", "required", "items", "enum",
		"default", "minimum", "maximum", "additionalProperties", "format":
		return true
	}
	return false
}

// ExportKind classifies what an unused export is.
type ExportKind string

const (
	ExportFunc ExportKind = "func"
	ExportType ExportKind = "type"
)

// UnusedExports reports exported functions and types that no production code
// outside their own file refers to.
//
// This is the gap the other checks do not cover, and it has already cost this
// repository once. prompt.Router — an exported type with WhenProvider,
// WhenDensity and WhenGroupActive hanging off it — was written, tested, and
// never wired to anything; the caller that should have used it hand-rolled a
// duplicate instead, and the two drifted until the copy silently dropped a
// whole section of the system prompt. Nothing failed. The unit test passed the
// entire time, because a test is a caller and the check that matters is whether
// anything else is.
//
// Methods are deliberately not examined. A method satisfying an interface is
// called through that interface and has no direct reference anywhere, so every
// adapter's Stream and Capability would be reported — dozens of findings, all
// wrong, which is how a check stops being read. Functions and types carry the
// same risk far more rarely, and the cost of missing a dead method is much
// lower than the cost of a checker nobody trusts.
//
// Test files do not count as callers, for the reason above: prompt.Router had a
// test and no user.
func (m *Module) UnusedExports(allow map[string]string) []Finding {
	type decl struct {
		kind ExportKind
		node ast.Node
		file string
	}
	declared := map[string]decl{}

	for path, file := range m.files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.FuncDecl:
				// Methods are skipped, as is main and the test entry points.
				if d.Recv != nil || !d.Name.IsExported() {
					return true
				}
				declared[d.Name.Name] = decl{kind: ExportFunc, node: d.Name, file: path}
			case *ast.TypeSpec:
				if !d.Name.IsExported() {
					return true
				}
				declared[d.Name.Name] = decl{kind: ExportType, node: d.Name, file: path}
			}
			return true
		})
	}

	var out []Finding
	for name, d := range declared {
		if _, excused := allow[name]; excused {
			continue
		}
		if m.identUsesOutside(name, d.file) > 0 {
			continue
		}
		out = append(out, Finding{
			Kind: "unused-export", Name: string(d.kind) + " " + name, Where: m.pos(d.node),
			Why: "exported and referenced by no production code outside its own file; " +
				"wire it to the caller it was written for, or delete it — an export with no user " +
				"is a promise the compiler cannot check and the next reader will assume is load-bearing",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// identUsesOutside counts uses of an identifier in every file except the one
// that declares it.
//
// Same-file use does not count: a helper called only from beside its own
// declaration is exactly the shape of a symbol that outlived its caller. Cross
// file use within the package does count, because that is live code — over
// exported perhaps, but not dead, and conflating the two would bury the
// findings that matter.
func (m *Module) identUsesOutside(name string, declFile string) int {
	count := 0
	for path, file := range m.files {
		if path == declFile {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				count++
			}
			return true
		})
	}
	return count
}
