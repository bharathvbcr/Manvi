package storeguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const storeImport = "github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"

// directStoreNews returns every call to dc/store's New made from a _test.go
// file under root, outside this package, as "file:line".
//
// It is the static half of the guard. Run only sees a leaked child that is
// still alive when the binary ends, and DevCouncil's store client ends an idle
// child after a minute (serveIdleTTL), so a client a test leaked early in a
// long package run can be gone before Run looks. Routing every test-built
// client through NewClient is what makes the Close impossible to forget, and
// this is what holds the routing.
func directStoreNews(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "vendor", "node_modules", "target":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "storeguard" {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		local := ""
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || p != storeImport {
				continue
			}
			local = "store"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		if local == "" || local == "_" {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "New" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
				pos := fset.Position(call.Pos())
				found = append(found, pos.Filename+":"+strconv.Itoa(pos.Line))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scanning %s: %v", root, err)
	}
	return found
}

// TestEveryTestStoreClientIsBuiltThroughNewClient fails on a test that builds
// a store client directly, so its Close is left to the test to remember.
func TestEveryTestStoreClientIsBuiltThroughNewClient(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	if found := directStoreNews(t, root); len(found) > 0 {
		t.Fatalf("%d test(s) build a store client with store.New; use storeguard.NewClient so it is closed:\n  %s",
			len(found), strings.Join(found, "\n  "))
	}
}

// TestTheScanFindsADirectStoreNew is the control: a scan that finds nothing
// must be one that could have found something, including under an alias.
func TestTheScanFindsADirectStoreNew(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a/plain_test.go": "package a\nimport \"" + storeImport + "\"\nfunc x() { _ = store.New(\"b\", \"d\") }\n",
		"b/alias_test.go": "package b\nimport dc \"" + storeImport + "\"\nfunc x() { _ = dc.New(\"b\", \"d\") }\n",
		"c/other_test.go": "package c\nimport store \"example.com/other/store\"\nfunc x() { _ = store.New() }\n",
		"d/prod.go":       "package d\nimport \"" + storeImport + "\"\nfunc x() { _ = store.New(\"b\", \"d\") }\n",
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found := directStoreNews(t, root)
	if len(found) != 2 {
		t.Fatalf("want the plain and the aliased test call (2), got %d: %v", len(found), found)
	}
	for _, f := range found {
		if !strings.Contains(f, "plain_test.go") && !strings.Contains(f, "alias_test.go") {
			t.Fatalf("flagged a call that is not dc/store's New in a test: %s", f)
		}
	}
}
