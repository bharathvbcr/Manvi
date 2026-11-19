package treefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckAliasRefusesADifferentlySpelledEntryAndLeavesTheExistingOneAlone(t *testing.T) {
	anchor, _ := tree(t)
	if err := os.Mkdir(filepath.Join(anchor, "Foo"), 0o750); err != nil {
		t.Fatal(err)
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	for _, alias := range []string{"foo", "FOO", "fOo"} {
		err := CheckAlias(root, alias)
		if err == nil || !errors.Is(err, ErrAliased) || !errors.Is(err, ErrRefused) {
			t.Errorf("CheckAlias(%q) = %v, want ErrAliased and ErrRefused", alias, err)
			continue
		}
		if !strings.Contains(err.Error(), "Foo") {
			t.Errorf("the refusal does not name the existing entry: %v", err)
		}
	}
	if err := CheckAlias(root, "Foo"); err != nil {
		t.Errorf("the spelling that is stored was refused: %v", err)
	}
	if err := CheckAlias(root, "Bar"); err != nil {
		t.Errorf("a name no entry resembles was refused: %v", err)
	}
	names, _ := os.ReadDir(anchor)
	if len(names) != 1 || names[0].Name() != "Foo" {
		t.Errorf("an entry was renamed or created: %v", names)
	}
}

// Both spellings stored on a case-sensitive file system stay readable by their
// own spelling: a stored entry is never an alias of itself.
func TestCheckAliasLeavesBothSpellingsOfAnExistingPairReachable(t *testing.T) {
	anchor, _ := tree(t)
	if err := os.Mkdir(filepath.Join(anchor, "Foo"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(anchor, "foo"), 0o750); err != nil {
		t.Skip("a case-insensitive file system cannot hold both")
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, n := range []string{"Foo", "foo"} {
		if err := CheckAlias(root, n); err != nil {
			t.Errorf("CheckAlias(%q) = %v", n, err)
		}
	}
}

// NFC and NFD spellings of one name are one entry on APFS. The package has no
// decomposition tables, so it asks the file system: where the two are the same
// file the second is refused, and where they are different files (Linux) both
// are allowed, because they are different names there.
func TestCheckAliasAsksTheFileSystemAboutNormalisationForms(t *testing.T) {
	const nfc, nfd = "café", "café"
	anchor, _ := tree(t)
	if err := os.Mkdir(filepath.Join(anchor, nfc), 0o750); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(filepath.Join(anchor, nfd))
	sameFile := statErr == nil
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	err = CheckAlias(root, nfd)
	if sameFile && !errors.Is(err, ErrAliased) {
		t.Fatalf("the file system reads %q as the existing %q, and the check let it through: %v", nfd, nfc, err)
	}
	if !sameFile && err != nil {
		t.Fatalf("the file system keeps %q and %q apart, and the check refused it: %v", nfd, nfc, err)
	}
	t.Logf("this file system treats NFC and NFD as one name: %v", sameFile)
}

func TestCheckAliasRefusesAnythingButOneSegment(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"", ".", "..", "a/b", "a\\b"} {
		if err := CheckAlias(root, name); !errors.Is(err, ErrRefused) {
			t.Errorf("CheckAlias(%q) = %v, want a refusal", name, err)
		}
	}
}

// Two writers creating the very same name at once are not aliases of each other:
// an entry spelled exactly so that appears between the listing and the look is
// the name itself.
func TestCheckAliasDoesNotRefuseTheExactNameAnotherWriterJustMade(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			name := "gitpulse" + string(rune('a'+i%26)) + string(rune('a'+i/26%26))
			go func() { _ = root.Mkdir(name, 0o750) }()
			if err := CheckAlias(root, name); err != nil {
				t.Errorf("CheckAlias(%q) = %v while another writer created that name", name, err)
				return
			}
		}
	}()
	<-done
}

// The folding is Unicode simple case folding, which is what APFS applies: the long
// s is an s and the Kelvin sign a k, and the Turkish dotless i is kept apart.
func TestCheckAliasFoldsWhatAPFSFolds(t *testing.T) {
	anchor, _ := tree(t)
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"status", "kilo", "cafÉ", "σ", "istanbul", "I"} {
		if err := os.Mkdir(filepath.Join(anchor, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, alias := range []string{"ſtatus", "Kilo", "café", "Σ", "ς"} {
		if err := CheckAlias(root, alias); !errors.Is(err, ErrAliased) {
			t.Errorf("CheckAlias(%q) = %v, want ErrAliased", alias, err)
		}
	}
	// No simple folding joins these, and APFS keeps them apart too.
	for _, apart := range []string{"ıstanbul", "İ"} {
		if err := CheckAlias(root, apart); err != nil {
			t.Errorf("CheckAlias(%q) = %v, want it allowed", apart, err)
		}
	}
}
