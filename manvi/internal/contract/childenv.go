package contract

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// A child process's environment is the one thing about it os/exec decides by
// omission: a Cmd whose Env is never assigned inherits everything this process
// has, the host's provider keys included. Every site that leaked them did so
// that way, or by handing os.Environ() over directly, and every one of them
// read as deliberate in review.
//
// ChildEnvSites is that review made total. Every exec.Command and
// exec.CommandContext in production code must be bound to a variable whose Env
// the same function assigns (or be an exec.Cmd literal with an Env field), and
// os.Environ() may be read only by package childenv — which is
// where the rule that no provider credential reaches a child is implemented —
// or at a site excused with a reason.

// ChildEnvScan is what ChildEnvSites examined and found.
type ChildEnvScan struct {
	// Spawns counts the exec.Command* calls and exec.Cmd literals examined, so
	// a scan that silently found none is distinguishable from a clean one.
	Spawns   int
	Findings []Finding
	// Environ lists, by file, the os.Environ() reads outside childenv,
	// excused or not, so a stale excuse can be detected.
	Environ map[string]bool
}

// ChildEnvSites checks every spawn site in the module. allowEnviron excuses an
// os.Environ() read in a file (module-relative path) that hands nothing to a
// child.
func (m *Module) ChildEnvSites(allowEnviron map[string]string) ChildEnvScan {
	scan := ChildEnvScan{Environ: map[string]bool{}}
	paths := make([]string, 0, len(m.files))
	for path := range m.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		file, ok := m.files[path]
		if !ok || file == nil {
			continue
		}
		rel, err := filepath.Rel(m.root, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		rel = filepath.ToSlash(rel)
		execName, osName := importName(file, "os/exec"), importName(file, "os")
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// A spawn is excused by an Env assignment to the variable it is
			// bound to, not by any Env assignment in the function: with two
			// children and one `cmd.Env =`, the other still inherits. bound
			// maps a spawn expression to the text of the place it is stored in;
			// it is filled when the enclosing statement is visited, which is
			// before the expression itself.
			type spawn struct {
				node  ast.Node
				bound string
				env   bool // an exec.Cmd literal that sets Env itself
			}
			var spawns []spawn
			bound := map[ast.Expr]string{}
			envAssigned := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if isPkgCall(x, execName, "Command", "CommandContext") {
						spawns = append(spawns, spawn{node: x, bound: bound[x]})
					}
					if isPkgCall(x, osName, "Environ") && !strings.HasPrefix(rel, "childenv/") {
						scan.Environ[rel] = true
						if _, excused := allowEnviron[rel]; !excused {
							scan.Findings = append(scan.Findings, Finding{
								Kind: "env", Name: "os.Environ", Where: m.pos(x),
								Why: "this process's whole environment, provider keys included; build a child's with childenv instead",
							})
						}
					}
				case *ast.CompositeLit:
					if sel, ok := x.Type.(*ast.SelectorExpr); ok && isIdent(sel.X, execName) && sel.Sel.Name == "Cmd" {
						sp := spawn{node: x, bound: bound[x]}
						for _, el := range x.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok && isIdent(kv.Key, "Env") {
								sp.env = true
							}
						}
						spawns = append(spawns, sp)
					}
				case *ast.AssignStmt:
					if len(x.Lhs) == len(x.Rhs) {
						for i, rhs := range x.Rhs {
							bound[unwrapSpawn(rhs)] = types.ExprString(x.Lhs[i])
						}
					}
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" {
							envAssigned[types.ExprString(sel.X)] = true
						}
					}
				case *ast.ValueSpec:
					if len(x.Names) == len(x.Values) {
						for i, v := range x.Values {
							bound[unwrapSpawn(v)] = x.Names[i].Name
						}
					}
				}
				return true
			})
			scan.Spawns += len(spawns)
			for _, sp := range spawns {
				if sp.env || (sp.bound != "" && envAssigned[sp.bound]) {
					continue
				}
				scan.Findings = append(scan.Findings, Finding{
					Kind: "spawn", Name: fn.Name.Name, Where: m.pos(sp.node),
					Why: "starts a child without assigning Env to the command it creates, so it inherits this process's provider keys",
				})
			}
		}
	}
	return scan
}

// unwrapSpawn strips the parentheses and address-of around a spawn expression,
// so `cmd := &exec.Cmd{...}` binds the literal, not the `&` around it.
func unwrapSpawn(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.UnaryExpr:
			if x.Op != token.AND {
				return e
			}
			e = x.X
		default:
			return e
		}
	}
}

// importName is the name file refers to path by, or "" when it does not
// import it.
func importName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		if p, err := strconv.Unquote(spec.Path.Value); err != nil || p != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

func isPkgCall(call *ast.CallExpr, pkg string, names ...string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || pkg == "" || !isIdent(sel.X, pkg) {
		return false
	}
	for _, name := range names {
		if sel.Sel.Name == name {
			return true
		}
	}
	return false
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
