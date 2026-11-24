package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	devcouncilModule = "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator"
	storeCapName     = "maxWorkbenchInput"
)

// pinnedStoreCap reads maxWorkbenchInput from the DevCouncil module this build
// resolves. The constant is unexported, so the source is the only place it can
// be read; a source that cannot be found or evaluated is an error, never a skip.
func pinnedStoreCap(t *testing.T) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-m", "-json", devcouncilModule).Output()
	if err != nil {
		t.Fatalf("go list -m %s: %v", devcouncilModule, err)
	}
	var mod struct{ Dir string }
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatalf("decoding go list output: %v", err)
	}
	if mod.Dir == "" {
		t.Fatalf("go list -m %s reported no directory", devcouncilModule)
	}
	path := filepath.Join(mod.Dir, "dc", "store", "workbench.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != storeCapName {
					continue
				}
				if i >= len(vs.Values) {
					t.Fatalf("%s in %s has no value expression", storeCapName, path)
				}
				var expr bytes.Buffer
				if err := printer.Fprint(&expr, fset, vs.Values[i]); err != nil {
					t.Fatalf("printing %s: %v", storeCapName, err)
				}
				tv, err := types.Eval(token.NewFileSet(), nil, token.NoPos, expr.String())
				if err != nil {
					t.Fatalf("evaluating %s = %q: %v", storeCapName, expr.String(), err)
				}
				v, ok := constant.Int64Val(constant.ToInt(tv.Value))
				if !ok {
					t.Fatalf("%s = %q is not an integer constant", storeCapName, expr.String())
				}
				return v
			}
		}
	}
	t.Fatalf("%s not found in %s", storeCapName, path)
	return 0
}

// maxProtocolRequest is a hand copy of the store's unexported cap. If they
// differ, the fit step either never runs while the store refuses the write, or
// trims output the store would have taken.
func TestMaxProtocolRequestMatchesTheStoreCap(t *testing.T) {
	got := pinnedStoreCap(t)
	if int64(maxProtocolRequest) != got {
		t.Fatalf("serve maxProtocolRequest = %d but DevCouncil %s = %d", maxProtocolRequest, storeCapName, got)
	}
}
