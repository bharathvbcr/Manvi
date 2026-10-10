package contract

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// An environment variable the harness reads is an interface, and the only
// place a reader finds out about it is a document. The flag catalogue answers
// for every setting reachable as MANVI_<KEY>, but the process also reads names
// that are not flags: credentials, endpoints, binary overrides, root
// resolution, terminal conventions. Those were read by the code and absent from
// every document, so a variable could be added, or its meaning changed, with
// nothing noticing.
//
// EnvReads is the grep for those reads, made total: it finds the names the
// production source reads and reports every read whose name it could not
// resolve, so a call site it cannot see is a number on the page, not a silent
// gap.

// envNameShape is a name this harness owns. Any string literal of this shape is
// treated as a variable name wherever it appears, because the ways of reading
// one (a loop over a list, a wrapper, a constant in another file) are more than
// a syntactic match on os.Getenv can follow.
var envNameShape = regexp.MustCompile(`^(MANVI|DEVCOUNCIL)_[A-Z0-9_]+$`)

// envReaders are the calls whose first argument is a variable name.
var envReaders = map[string]map[string]bool{
	"os": {"Getenv": true, "LookupEnv": true},
}

// EnvRead is one variable name the source reads.
type EnvRead struct {
	Name  string
	Where string // file:line of the first sighting
}

// EnvSite is a read whose name could not be resolved to a literal.
type EnvSite struct {
	Where string // file:line
	Expr  string // the argument as written
}

// EnvForward is a package-level allowlist whose members are read in a loop and
// handed to a child process. The members are policy, not interface: the list
// is documented by naming where it lives.
type EnvForward struct {
	Var   string
	Where string
}

// EnvScan is everything EnvReads found.
type EnvScan struct {
	Reads      []EnvRead
	Unresolved []EnvSite
	Forwarded  []EnvForward
}

// envIndex is what resolving a name needs from the rest of the package: its
// string constants and its string-slice variables, per directory.
type envIndex struct {
	consts map[string]map[string]string
	lists  map[string]map[string][]string
}

// EnvReads reports the environment variable names the production source reads.
func (m *Module) EnvReads() EnvScan {
	modPath := goModulePath(m.root)
	idx := m.indexEnv()
	wrappers := m.envWrappers()

	seen := map[string]EnvRead{}
	add := func(name, where string) {
		if _, ok := seen[name]; !ok {
			seen[name] = EnvRead{Name: name, Where: where}
		}
	}
	var scan EnvScan
	forwarded := map[string]EnvForward{}

	for _, path := range m.sortedFiles() {
		file := m.files[path]
		dir := filepath.Dir(path)
		imports := fileImports(file)

		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch n := n.(type) {
			case *ast.BasicLit:
				if v, ok := stringLit(n); ok && envNameShape.MatchString(v) {
					add(v, m.pos(n))
				}
			case *ast.KeyValueExpr:
				// credentials.Requirement{EnvVars: []string{...}}: the names are
				// listed where the provider is declared.
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == "EnvVars" {
					if lit, ok := n.Value.(*ast.CompositeLit); ok {
						for _, el := range lit.Elts {
							if v, ok := stringExpr(el); ok {
								add(v, m.pos(el))
							}
						}
					}
				}
			case *ast.CallExpr:
				arg, isReader := m.envReaderArg(n, wrappers)
				if !isReader {
					return true
				}
				names, how := resolveEnvArg(arg, stack, dir, imports, idx, modPath, m.root)
				switch how {
				case envResolved:
					for _, v := range names {
						add(v, m.pos(arg))
					}
				case envForwarded:
					fwd := EnvForward{Var: names[0], Where: m.pos(arg)}
					forwarded[fwd.Var] = fwd
				case envWrapperParam:
					// A wrapper reading its own parameter: the call sites that
					// feed it are the reads, and they are resolved on their own.
				default:
					scan.Unresolved = append(scan.Unresolved, EnvSite{Where: m.pos(arg), Expr: exprString(arg)})
				}
			}
			return true
		})
	}

	for _, r := range seen {
		scan.Reads = append(scan.Reads, r)
	}
	sort.Slice(scan.Reads, func(i, j int) bool { return scan.Reads[i].Name < scan.Reads[j].Name })
	for _, f := range forwarded {
		scan.Forwarded = append(scan.Forwarded, f)
	}
	sort.Slice(scan.Forwarded, func(i, j int) bool { return scan.Forwarded[i].Var < scan.Forwarded[j].Var })
	sort.Slice(scan.Unresolved, func(i, j int) bool { return scan.Unresolved[i].Where < scan.Unresolved[j].Where })
	return scan
}

// UndocumentedEnv returns the reads whose names do not appear in doc, and the
// forwarded allowlists whose variable is not named there. exempt lists names
// that are platform conventions rather than this harness's interface.
func UndocumentedEnv(scan EnvScan, doc string, exempt map[string]bool) []string {
	var out []string
	for _, r := range scan.Reads {
		if exempt[r.Name] || mentions(doc, r.Name) {
			continue
		}
		out = append(out, fmt.Sprintf("%s (read at %s)", r.Name, r.Where))
	}
	for _, f := range scan.Forwarded {
		if mentions(doc, f.Var) {
			continue
		}
		out = append(out, fmt.Sprintf("%s: an allowlist forwarded to child processes (at %s)", f.Var, f.Where))
	}
	return out
}

// mentions reports whether name appears in doc as a whole token, so that
// MANVI_FX is not satisfied by MANVI_FX_FOO.
func mentions(doc, name string) bool {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_])`).MatchString(doc)
}

// envReaderArg returns the name argument of a call that reads the environment,
// directly (os.Getenv) or through a wrapper that passes its parameter to one.
func (m *Module) envReaderArg(call *ast.CallExpr, wrappers map[string]int) (ast.Expr, bool) {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok && envReaders[pkg.Name][fun.Sel.Name] && len(call.Args) > 0 {
			return call.Args[0], true
		}
	case *ast.Ident:
		if i, ok := wrappers[fun.Name]; ok && i < len(call.Args) {
			return call.Args[i], true
		}
	}
	return nil, false
}

// envWrappers finds the functions that hand one of their own parameters to an
// environment reader — env(key, fallback), toolBinary(envKey, name) — by the
// shape of their body rather than by a list, so a new wrapper is found too. The
// result maps the function name to the index of that parameter.
func (m *Module) envWrappers() map[string]int {
	out := map[string]int{}
	for _, path := range m.sortedFiles() {
		for _, decl := range m.files[path].Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			params := map[string]int{}
			i := 0
			for _, field := range fn.Type.Params.List {
				for _, name := range field.Names {
					params[name.Name] = i
					i++
				}
				if len(field.Names) == 0 {
					i++
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || !envReaders[pkg.Name][sel.Sel.Name] {
					return true
				}
				if id, ok := call.Args[0].(*ast.Ident); ok {
					if pi, isParam := params[id.Name]; isParam {
						out[fn.Name.Name] = pi
					}
				}
				return true
			})
		}
	}
	return out
}

type envResolution int

const (
	envUnresolved envResolution = iota
	envResolved
	envForwarded
	envWrapperParam
)

// resolveEnvArg turns the name argument of a read into literals. It follows a
// literal, a string constant in the same package or in another package of this
// module, and a loop variable ranging over a literal list or a package-level
// string slice. Anything else is unresolved, and the caller reports it.
func resolveEnvArg(arg ast.Expr, stack []ast.Node, dir string, imports map[string]string, idx envIndex, modPath, root string) ([]string, envResolution) {
	if v, ok := stringExpr(arg); ok {
		return []string{v}, envResolved
	}
	switch a := arg.(type) {
	case *ast.Ident:
		if v, ok := idx.consts[dir][a.Name]; ok {
			return []string{v}, envResolved
		}
		for i := len(stack) - 1; i >= 0; i-- {
			switch s := stack[i].(type) {
			case *ast.RangeStmt:
				val, ok := s.Value.(*ast.Ident)
				if !ok || val.Name != a.Name {
					continue
				}
				if lit, ok := s.X.(*ast.CompositeLit); ok {
					var names []string
					for _, el := range lit.Elts {
						v, ok := stringExpr(el)
						if !ok {
							return nil, envUnresolved
						}
						names = append(names, v)
					}
					return names, envResolved
				}
				if id, ok := s.X.(*ast.Ident); ok {
					if strings.HasSuffix(id.Name, "Allowlist") {
						if _, found := idx.lists[dir][id.Name]; found {
							return []string{id.Name}, envForwarded
						}
					}
					if list, found := idx.lists[dir][id.Name]; found {
						return list, envResolved
					}
				}
				return nil, envUnresolved
			case *ast.FuncDecl:
				for _, field := range s.Type.Params.List {
					for _, name := range field.Names {
						if name.Name == a.Name {
							return nil, envWrapperParam
						}
					}
				}
			}
		}
	case *ast.SelectorExpr:
		pkg, ok := a.X.(*ast.Ident)
		if !ok {
			break
		}
		path, ok := imports[pkg.Name]
		if !ok || modPath == "" || !strings.HasPrefix(path, modPath+"/") {
			break
		}
		target := filepath.Join(root, strings.TrimPrefix(path, modPath+"/"))
		if v, ok := idx.consts[target][a.Sel.Name]; ok {
			return []string{v}, envResolved
		}
	}
	return nil, envUnresolved
}

func (m *Module) indexEnv() envIndex {
	idx := envIndex{consts: map[string]map[string]string{}, lists: map[string]map[string][]string{}}
	for path, file := range m.files {
		dir := filepath.Dir(path)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						break
					}
					if v, ok := stringExpr(vs.Values[i]); ok && gen.Tok == token.CONST {
						if idx.consts[dir] == nil {
							idx.consts[dir] = map[string]string{}
						}
						idx.consts[dir][name.Name] = v
					}
					if lit, ok := vs.Values[i].(*ast.CompositeLit); ok {
						var list []string
						complete := true
						for _, el := range lit.Elts {
							v, ok := stringExpr(el)
							if !ok {
								complete = false
								break
							}
							list = append(list, v)
						}
						if complete && len(list) > 0 {
							if idx.lists[dir] == nil {
								idx.lists[dir] = map[string][]string{}
							}
							idx.lists[dir][name.Name] = list
						}
					}
				}
			}
		}
	}
	return idx
}

func (m *Module) sortedFiles() []string {
	paths := make([]string, 0, len(m.files))
	for p := range m.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func fileImports(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = path
	}
	return out
}

func stringLit(lit *ast.BasicLit) (string, bool) {
	if lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

func stringExpr(e ast.Expr) (string, bool) {
	if lit, ok := e.(*ast.BasicLit); ok {
		return stringLit(lit)
	}
	return "", false
}

func exprString(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		return exprString(e.Fun) + "(...)"
	case *ast.IndexExpr:
		return exprString(e.X) + "[...]"
	case *ast.BasicLit:
		return e.Value
	}
	return fmt.Sprintf("%T", e)
}

// goModulePath reads the module path from root/go.mod, or "" when there is none.
func goModulePath(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
