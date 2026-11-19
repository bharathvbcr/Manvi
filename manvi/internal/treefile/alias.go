package treefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// ErrAliased marks a name refused because it would alias an entry that is
// already there under a different spelling. On a case-insensitive file system
// (APFS and NTFS by default) "Foo" and "foo" are one directory entry, so a
// second writer under the other spelling lands in the first one's place, and a
// reader of either gets the other's data. It is also a refusal (ErrRefused).
var ErrAliased = errors.New("aliases an existing entry")

// maxAliasScan bounds how many entries of one directory CheckAlias reads. A
// directory with more is refused rather than checked in part, because a check
// that stopped early would answer "no alias" for names it never compared.
const maxAliasScan = 1 << 16

// CheckAlias refuses, wrapping ErrAliased, a name that is not an entry of dir
// but would alias one: an entry that folds equal to it under Unicode simple case
// folding (strings.EqualFold, the folding the catalog already uses for field
// names: the long s is an s, the Kelvin sign a k, and the Turkish dotless i has no
// simple folding to i, as on APFS), or an entry the file system reads as the same
// name by a rule that folding does not know. NFC and NFD spellings of one name are
// the case: telling them apart needs the Unicode decomposition tables, which the
// standard library does not carry, so the file system is asked instead. An entry spelled exactly as name is not an
// alias, so an existing entry stays reachable by the spelling it was stored
// under, and nothing is ever renamed. name is one segment of dir.
//
// It is a check made before the write, under whatever lock the caller holds, and
// is not atomic with it: two processes creating two spellings at the same
// instant can both pass.
func CheckAlias(dir *os.Root, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return refusal(name, "is not a single path segment")
	}
	existing, exact, err := scanAlias(dir, name)
	if err != nil || exact {
		return err
	}
	if existing != "" {
		return aliasRefusal(name, existing)
	}
	// Not in the listing under any spelling this package folds. On a file system
	// that treats other spellings as the same name, an entry nonetheless answers
	// to it.
	if _, err := dir.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// Something answers to the name. Either another writer made exactly this entry
	// between the listing and the look, which is no alias, or the file system
	// reads another spelling as this one. The listing is taken again to tell them
	// apart: an entry spelled exactly so now is the first.
	existing, exact, err = scanAlias(dir, name)
	if err != nil || exact {
		return err
	}
	if existing != "" {
		return aliasRefusal(name, existing)
	}
	return fmt.Errorf("%w: %w: %q is not spelled like any entry of the directory, yet the file system reads it as one, "+
		"so it was refused", ErrRefused, ErrAliased, name)
}

func aliasRefusal(name, existing string) error {
	return fmt.Errorf("%w: %w: %q would alias the existing entry %q, which differs from it only in case, so it was refused",
		ErrRefused, ErrAliased, name, existing)
}

// scanAlias lists dir once. exact reports an entry spelled exactly as name;
// otherwise existing is the first entry that folds equal to it, or empty.
func scanAlias(dir *os.Root, name string) (existing string, exact bool, err error) {
	d, err := dir.Open(".")
	if err != nil {
		return "", false, err
	}
	names, err := d.Readdirnames(maxAliasScan + 1)
	closeErr := d.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return "", false, closeErr
	}
	if len(names) > maxAliasScan {
		return "", false, fmt.Errorf("%w: %q: the directory holds more than %d entries, so whether the name aliases one cannot be checked",
			ErrRefused, name, maxAliasScan)
	}
	for _, have := range names {
		if have == name {
			return "", true, nil
		}
		if existing == "" && strings.EqualFold(have, name) {
			existing = have
		}
	}
	return existing, false, nil
}
