package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Every field of Status must be set by production code. A field only a test
// sets is a segment the bar can draw and no session can ever show: it reads as
// a feature in the render code and is dead in the product.
//
// "Set" is an assignment to X.Status.Field, or a Status composite literal that
// names the field, in a file of this package that is not a test. Status is not
// constructed outside the package.
func TestEveryStatusFieldIsSetByProductionCode(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var declared []string
	set := map[string]bool{}
	isStatusSel := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Status"
	}
	markTarget := func(e ast.Expr) {
		if sel, ok := e.(*ast.SelectorExpr); ok && isStatusSel(sel.X) {
			set[sel.Sel.Name] = true
		}
	}
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeSpec:
				if st, ok := n.Type.(*ast.StructType); ok && n.Name.Name == "Status" {
					for _, f := range st.Fields.List {
						for _, id := range f.Names {
							declared = append(declared, id.Name)
						}
					}
				}
			case *ast.AssignStmt:
				for _, l := range n.Lhs {
					markTarget(l)
				}
			case *ast.IncDecStmt:
				markTarget(n.X)
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && id.Name == "Status" {
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok {
								set[k.Name] = true
							}
						}
					}
				}
			}
			return true
		})
	}
	if len(declared) == 0 {
		t.Fatal("found no Status fields; the scan proves nothing")
	}
	for _, f := range declared {
		if !set[f] {
			t.Errorf("Status.%s is never set by production code: drop the field and its rendering, or set it", f)
		}
	}
}
