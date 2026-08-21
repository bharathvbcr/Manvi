package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"manvi/flags"
	"manvi/policy"
)

func TestAdversarialYAMLConfigs(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantModel string
		wantRigor bool
		wantErr   bool
	}{
		{
			name: "deeply_nested_with_mixed_arrays_and_dicts",
			yaml: `
# Root comment
commands:
  lint:
    - npm run lint
    - eslint .
  test: []
execution:
  deep:
    level:
      nested:
        value: 123
  max_repair_attempts: 5
integrations:
  cli_agents:
    agents:
      reviewer:
        model: 'qwen3.8:27b-mlx'
        timeout: 100
models:
  provider: 'openrouter'
verification:
  rigor:
    enabled: yes
llm:
  local:
    model: 'deepseek-coder'
    base_url: 'http://127.0.0.1:11434/v1#local'
`,
			wantModel: "deepseek-coder",
			wantRigor: true,
			wantErr:   false,
		},
		{
			name: "boolean_representations",
			yaml: `
verify:
  rigor:
    enabled: 'on'
harness:
  init:
    enabled: 'yes'
`,
			wantRigor: true,
			wantErr:   false,
		},
		{
			name: "typo_in_harness_namespace_fails",
			yaml: `
llm:
  local:
    modle_typo: 'something'
`,
			wantErr: true,
		},
		{
			name: "empty_and_comment_only_yaml",
			yaml: `
# Just comments
# nothing else

`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			cfgPath := filepath.Join(tmp, "config.yaml")
			if err := os.WriteFile(cfgPath, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}

			reg, err := flags.NewHarnessRegistry(cfgPath)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s, got nil", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.name, err)
			}

			if tc.wantModel != "" {
				m, _, err := reg.String(flags.LLMLocalModel)
				if err != nil || m != tc.wantModel {
					t.Errorf("model = %q, want %q", m, tc.wantModel)
				}
			}
			if tc.wantRigor {
				r, _, err := reg.Bool(flags.VerifyRigorEnabled)
				if err != nil || !r {
					t.Errorf("verify.rigor.enabled = %v, want true", r)
				}
			}
		})
	}
}

func TestConcurrentProjectRootAndPaths(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create 10 subdirectories
	var subs []string
	for i := 0; i < 10; i++ {
		sub := filepath.Join(root, "pkg", "sub", string(rune('a'+i)))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		subs = append(subs, sub)
	}

	var wg sync.WaitGroup
	for _, sub := range subs {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				found := findProjectRoot(dir)
				if found != root {
					t.Errorf("findProjectRoot(%q) = %q; want %q", dir, found, root)
				}
				resolved := resolveCLIPath(root, dir, "calc.go")
				if !strings.HasPrefix(resolved, root) {
					t.Errorf("resolveCLIPath returned path outside root: %q", resolved)
				}
			}
		}(sub)
	}
	wg.Wait()
}

func TestPolicyBoundaryContainmentStress(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	// Test 1: Traversals escaping root
	for _, escaping := range []string{
		"../escape.go",
		"../../escape.go",
		"../../../etc/passwd",
		"/etc/passwd",
		"/var/log/system.log",
		"a/b/../../../../escape.go",
	} {
		_, outside := policy.NormalizeRepoPath(root, escaping)
		if !outside {
			t.Errorf("NormalizeRepoPath(%q) should be outside root %q", escaping, root)
		}
	}

	// Test 2: Valid paths inside root
	for _, inside := range []string{
		"main.go",
		"pkg/calc/calc.go",
		"./pkg/calc/calc.go",
		"a/b/c/d/e/file.go",
		"a/b/../b/c/file.go",
	} {
		posix, outside := policy.NormalizeRepoPath(root, inside)
		if outside {
			t.Errorf("NormalizeRepoPath(%q) should be inside root %q, got outside", inside, root)
		}
		if strings.HasPrefix(posix, "/") || strings.HasPrefix(posix, ".") {
			t.Errorf("posix path %q should be clean repo-relative", posix)
		}
	}
}

// TestCorruptedDevMapIsRefusedNotIgnored.
//
// This used to assert that buildGate carried on regardless, and it passed for a
// reason that had nothing to do with robustness: buildGate did not read the
// code graph at all. Only nativeToolsWith did, and it refused — so a corrupt
// index already stopped a run from starting, while `manvi check` went on
// answering from a gate with no navigation index at all.
//
// Carrying on is the wrong behaviour to want here. With no map the neighbour
// rule falls back to allowing writes beside a planned file, so ignoring a
// corrupt index does not degrade the check, it widens it — and reports the
// wider answer as though the index had been consulted. Both gates now refuse,
// and they refuse identically.
func TestCorruptedDevMapIsRefusedNotIgnored(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	dcDir := filepath.Join(root, ".devcouncil")
	if err := os.Mkdir(dcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	graphFile := filepath.Join(dcDir, "code_graph.json")
	if err := os.WriteFile(graphFile, []byte("{ malformed json"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanup := setProjectRootForTest(root)
	defer cleanup()

	reg, err := flags.NewHarnessRegistry(configPath())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	_, _, err = buildGate(reg)
	if err == nil {
		t.Fatal("a corrupt code graph was accepted; the neighbour rule would have run wide and said nothing")
	}
	if !strings.Contains(err.Error(), "code_graph.json") {
		t.Errorf("refusal = %v, want it to name the file that must be rebuilt", err)
	}

	// The tool surface, which is what actually judges an agent's writes, must
	// refuse the same input. Two gates that disagree about a corrupt index is
	// the divergence this pairing exists to rule out.
	if _, _, err := nativeTools(reg); err == nil {
		t.Fatal("the tool surface accepted a corrupt code graph the gate refused")
	}
}

// TestAbsentDevMapIsNotAnError is the other half. A repository that has never
// been indexed is the ordinary case, not a fault, and refusing it would break
// every run that has not built the map yet.
func TestAbsentDevMapIsNotAnError(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	cleanup := setProjectRootForTest(root)
	defer cleanup()

	reg, err := flags.NewHarnessRegistry(configPath())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	g, m, err := buildGate(reg)
	if err != nil {
		t.Fatalf("buildGate with no index: %v", err)
	}
	if m != nil {
		t.Fatalf("no index on disk, but one was loaded: %+v", m)
	}
	// The nil-interface trap: a nil *repomap.Map stored in a policy.SubsystemMap
	// is an interface that is not nil, and the policy layer tests that interface
	// to decide whether it has a map to consult. Storing it would turn "no
	// index" into "an index that answers nothing" for every neighbour decision.
	if g.Subsystems != nil {
		t.Fatal("a nil map was stored as a non-nil SubsystemMap; the neighbour rule would consult an index that is not there")
	}

	decision, err := g.EvaluateWrite("main.go", nil, "write")
	if err != nil {
		t.Fatalf("EvaluateWrite failed: %v", err)
	}
	t.Logf("decision with no index: %+v", decision)
}

func TestCLICommandsRunCleanly(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanup := setProjectRootForTest(root)
	defer cleanup()

	var out, notes bytes.Buffer
	if err := run(&out, &notes, []string{"flags"}); err != nil {
		t.Fatalf("run flags: %v", err)
	}
	if !strings.Contains(out.String(), "harness.posture") {
		t.Errorf("output missing flags: %s", out.String())
	}

	out.Reset()
	notes.Reset()
	if err := run(&out, &notes, []string{"doctor"}); err != nil {
		t.Fatalf("run doctor: %v", err)
	}
	if !strings.Contains(out.String(), "manvi doctor") {
		t.Errorf("output missing doctor header: %s", out.String())
	}
}
