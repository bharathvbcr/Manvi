package contract

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Prose that names the current DevCouncil or gusset pin is a declaration that
// scripts/module-pins.txt decides. docs/ARCHITECTURE.md once said the pin was
// 1f6d5452 while the file pinned d890d437, two pin moves later, and no gate
// compared the two (ft-7597dcf9).
//
// Only statements of the CURRENT pin are checked: "DevCouncil pin (`hash`)" and
// "`scripts/module-pins.txt`, DevCouncil `hash`". Historical hashes (a fix
// landed in `c6b48a28`, the pin at a release tag) name past commits on purpose.
var pinClaim = regexp.MustCompile("(?i)(?:(DevCouncil|gusset) )?pins? \\(?`([0-9a-f]{7,40})`" +
	"|module-pins\\.txt`,? (DevCouncil|gusset) `([0-9a-f]{7,40})`")

// pinsFromFile reads module name -> full commit from scripts/module-pins.txt.
func pinsFromFile(t *testing.T) map[string]string {
	t.Helper()
	body := readDoc(t, filepath.Join(docRoot, "scripts", "module-pins.txt"))
	pins := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 3 {
			t.Fatalf("scripts/module-pins.txt: want 3 tab-separated columns, got %d in %q", len(cols), line)
		}
		pins[strings.ToLower(cols[0])] = cols[2]
	}
	for _, m := range []string{"devcouncil", "gusset"} {
		if len(pins[m]) != 40 {
			t.Fatalf("scripts/module-pins.txt: %s is not pinned to a full commit: %q", m, pins[m])
		}
	}
	return pins
}

func TestPinHashesStatedInDocsMatchModulePins(t *testing.T) {
	pins := pinsFromFile(t)
	checked := 0
	for _, path := range docFiles(t) {
		for i, line := range strings.Split(readDoc(t, path), "\n") {
			for _, m := range pinClaim.FindAllStringSubmatch(line, -1) {
				mod, hash := m[1], m[2]
				if mod == "" {
					mod, hash = m[3], m[4]
				}
				if mod == "" {
					mod = "DevCouncil" // a bare "pin (`hash`)" is the DevCouncil pin
				}
				checked++
				if full := pins[strings.ToLower(mod)]; !strings.HasPrefix(full, strings.ToLower(hash)) {
					t.Errorf("%s:%d names %s pin %s, but scripts/module-pins.txt pins %s",
						rel(path), i+1, mod, hash, full)
				}
			}
		}
	}
	t.Logf("checked %d current-pin statements in docs", checked)
}

// The pin-claim matcher must actually fire, or the test above passes by never
// looking.
func TestPinClaimMatcherRecognisesBothForms(t *testing.T) {
	for _, s := range []string{
		"DevCouncil pin (`1f6d5452`) carries",
		"(`scripts/module-pins.txt`, DevCouncil `1f6d5452`) carries",
		"the gusset pin `e561435a` is",
	} {
		if !pinClaim.MatchString(s) {
			t.Errorf("pin claim %q was not recognised", s)
		}
	}
	for _, s := range []string{
		"fixed in DevCouncil `c6b48a28`, an ancestor of the pin",
		"moved from DevCouncil `a29a11b`, which carried",
	} {
		if pinClaim.MatchString(s) {
			t.Errorf("historical statement %q was treated as a current-pin claim", s)
		}
	}
}

// The workbench schema is a Rust constant (rust/dc-store/src/workbench/mod.rs
// WORKBENCH_SCHEMA), not exported by any Go package Manvi imports, and CI may
// not read sibling checkouts. So the number is read BY HAND at a named pin and
// recorded here; moving the pin fails this test until it is re-read.
const (
	schemaVerifiedAtPin  = "d9c7a512c8e9d42a73b83ec7afa3c36987353fd4"
	workbenchSchemaAtPin = 17 // WORKBENCH_SCHEMA, rust/dc-store/src/workbench/mod.rs:36
	dcStoreSchemaAtPin   = 11 // SCHEMA_VERSION, rust/dc-store/src/lib.rs:61
)

var schemaWords = map[string]int{"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17}

func TestWorkbenchSchemaStatedInDocsMatchesPin(t *testing.T) {
	if got := pinsFromFile(t)["devcouncil"]; got != schemaVerifiedAtPin {
		t.Fatalf("scripts/module-pins.txt pins DevCouncil %s, but the workbench schema was last read at %s.\n"+
			"Re-read WORKBENCH_SCHEMA (rust/dc-store/src/workbench/mod.rs) and SCHEMA_VERSION (rust/dc-store/src/lib.rs) "+
			"at the new pin, then update the constants in this file and the statements in docs/ARCHITECTURE.md and docs/WORKBENCH.md.",
			got, schemaVerifiedAtPin)
	}
	arch := readDoc(t, filepath.Join(docRoot, "docs", "ARCHITECTURE.md"))
	m := regexp.MustCompile(`pin \(` + "`scripts/module-pins.txt`" + `\) carries workbench schema (\w+)`).FindStringSubmatch(arch)
	if m == nil {
		t.Fatal("docs/ARCHITECTURE.md no longer states which workbench schema the pin carries")
	}
	if schemaWords[strings.ToLower(m[1])] != workbenchSchemaAtPin {
		t.Errorf("docs/ARCHITECTURE.md says workbench schema %q; WORKBENCH_SCHEMA at the pin is %d", m[1], workbenchSchemaAtPin)
	}
	wb := readDoc(t, filepath.Join(docRoot, "docs", "WORKBENCH.md"))
	w := regexp.MustCompile("(?s)release pin\n\\(`scripts/module-pins.txt`, DevCouncil\\) carries workbench (\\d+) with\n`dc-store` (\\d+)").FindStringSubmatch(wb)
	if w == nil {
		t.Fatal("docs/WORKBENCH.md no longer states the workbench and dc-store schema the release pin carries")
	}
	if w[1] != strconv.Itoa(workbenchSchemaAtPin) || w[2] != strconv.Itoa(dcStoreSchemaAtPin) {
		t.Errorf("docs/WORKBENCH.md says workbench %s / dc-store %s; the pin has %d / %d",
			w[1], w[2], workbenchSchemaAtPin, dcStoreSchemaAtPin)
	}
}
