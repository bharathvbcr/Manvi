package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in safefs_test.go all swap a directory that EXISTED when the
// target was pinned, so they never exercised the components the write creates
// for itself. Those were the unpinned ones, and they were the escape: pin
// "newdir/payload", replace "newdir" with a symbolic link while the approval
// prompt is open, and the write landed wherever the link pointed while the
// harness reported success.
//
// Everything below writes only inside t.TempDir(). The "outside" tree stands
// in for anything the repository must never reach — including a fake home
// directory with a fake authorized_keys — and every test asserts that tree is
// byte-for-byte unchanged, not merely that an error came back.

// treeSnapshot records every path under dir with its content (or link target),
// so a test can prove nothing outside the repository moved.
func treeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "symlink:" + target
		case fi.IsDir():
			out[rel] = "dir"
		case fi.Mode().IsRegular():
			data, readErr := os.ReadFile(p)
			if readErr != nil {
				return readErr
			}
			out[rel] = "file:" + string(data)
		default:
			out[rel] = "other:" + fi.Mode().String()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", dir, err)
	}
	return out
}

func assertUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := treeSnapshot(t, dir)
	for k, v := range after {
		if old, ok := before[k]; !ok {
			t.Fatalf("CONTAINMENT ESCAPED: %q appeared outside the repository (%s)", k, v)
		} else if old != v {
			t.Fatalf("CONTAINMENT ESCAPED: %q outside the repository changed from %q to %q", k, old, v)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			t.Fatalf("CONTAINMENT ESCAPED: %q outside the repository was deleted", k)
		}
	}
}

// TestWriteRefusesSwapOfADirectoryItCreatesItself is the headline
// reproduction. One missing component is enough: "newdir" does not exist when
// policy evaluates "newdir/payload.txt", so nothing about it is pinned, and
// the pre-fix Write created it with os.Mkdir, ignored EEXIST when it found a
// symbolic link instead, and then opened the leaf by whole path.
func TestWriteRefusesSwapOfADirectoryItCreatesItself(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "payload.txt"), []byte("PRE-EXISTING\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, outside)

	pinned, err := pinWriteTarget(root, "newdir/payload.txt")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}

	// The approval dialog is open. The attacker plants the link.
	if err := os.Symlink(outside, filepath.Join(root, "newdir")); err != nil {
		t.Fatal(err)
	}

	if err := pinned.Write([]byte("PAYLOAD-OUTSIDE-REPO"), 0o644); err == nil {
		t.Fatal("Write reported success through a directory it created and never verified")
	}
	assertUnchanged(t, outside, before)
}

// TestWriteRefusesSwapAtEveryDepth sweeps the swap across every component of
// a four-deep path, for every possible boundary between "existed at pin time"
// and "created by the write". Verifying one extra level would leave most of
// this matrix red.
func TestWriteRefusesSwapAtEveryDepth(t *testing.T) {
	parts := []string{"d0", "d1", "d2", "d3"}
	for existing := 0; existing <= len(parts); existing++ {
		for swap := 0; swap < len(parts); swap++ {
			name := fmt.Sprintf("existing%d_swap%d", existing, swap)
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				outside := t.TempDir()
				rel := strings.Join(parts, "/") + "/leaf.txt"

				// Pre-create the first `existing` components.
				if existing > 0 {
					if err := os.MkdirAll(filepath.Join(append([]string{root}, parts[:existing]...)...), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				// A file the attacker hopes to clobber, at the name the write
				// would land on if it followed the link.
				victimDir := filepath.Join(append([]string{outside}, parts[swap+1:]...)...)
				if err := os.MkdirAll(victimDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(victimDir, "leaf.txt"), []byte("innocent\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				before := treeSnapshot(t, outside)

				pinned, err := pinWriteTarget(root, rel)
				if err != nil {
					t.Fatalf("pin: %v", err)
				}

				// Swap component `swap` for a link out of the repository.
				swapped := filepath.Join(append([]string{root}, parts[:swap+1]...)...)
				if err := os.RemoveAll(swapped); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(swapped), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, swapped); err != nil {
					t.Fatal(err)
				}

				if err := pinned.Write([]byte("PWNED"), 0o644); err == nil {
					t.Fatalf("Write reported success with component %d swapped (%d pinned)", swap, existing)
				}
				assertUnchanged(t, outside, before)
			})
		}
	}
}

// TestWriteRefusesSwapOfCreatedComponentToFakeHome is the concrete harm: the
// swap aims at a stand-in ~/.ssh/authorized_keys (inside t.TempDir(), never
// the real home directory) and the payload is an attacker key.
func TestWriteRefusesSwapOfCreatedComponentToFakeHome(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(ssh, "authorized_keys")
	if err := os.WriteFile(keys, []byte("ssh-ed25519 OWNER-KEY owner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, home)

	pinned, err := pinWriteTarget(root, "build/cache/authorized_keys")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	// Two components missing at pin time; the attacker only needs the first.
	if err := os.Symlink(ssh, filepath.Join(root, "build")); err != nil {
		t.Fatal(err)
	}

	err = pinned.Write([]byte("ssh-ed25519 ATTACKER-KEY attacker\n"), 0o600)
	if err == nil {
		t.Fatal("Write reported success into a symlinked stand-in home directory")
	}
	assertUnchanged(t, home, before)

	data, readErr := os.ReadFile(keys)
	if readErr != nil || string(data) != "ssh-ed25519 OWNER-KEY owner\n" {
		t.Fatalf("the stand-in authorized_keys was modified: %q (%v)", data, readErr)
	}
}

// TestWriteRefusesEveryHostileComponentShape: the swapped-in object does not
// have to be a symbolic link to be wrong. Each shape must refuse, and none may
// touch anything outside the repository.
func TestWriteRefusesEveryHostileComponentShape(t *testing.T) {
	shapes := []struct {
		name  string
		plant func(t *testing.T, at, outside string)
	}{
		{"symlink_outside", func(t *testing.T, at, outside string) {
			if err := os.Symlink(outside, at); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink_relative_outside", func(t *testing.T, at, outside string) {
			rel, err := filepath.Rel(filepath.Dir(at), outside)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(rel, at); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling_symlink", func(t *testing.T, at, outside string) {
			if err := os.Symlink(filepath.Join(outside, "nowhere"), at); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink_inside_repo", func(t *testing.T, at, outside string) {
			elsewhere := filepath.Join(filepath.Dir(at), "elsewhere")
			if err := os.MkdirAll(elsewhere, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("elsewhere", at); err != nil {
				t.Fatal(err)
			}
		}},
		{"regular_file", func(t *testing.T, at, outside string) {
			if err := os.WriteFile(at, []byte("not a directory\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, at, outside string) {
			if err := syscall_Mkfifo(at, 0o644); err != nil {
				t.Skipf("cannot create a fifo here: %v", err)
			}
		}},
		{"real_directory", func(t *testing.T, at, outside string) {
			if err := os.Mkdir(at, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "leaf.txt"), []byte("innocent\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := treeSnapshot(t, outside)

			pinned, err := pinWriteTarget(root, "created/leaf.txt")
			if err != nil {
				t.Fatalf("pin: %v", err)
			}
			shape.plant(t, filepath.Join(root, "created"), outside)

			err = pinned.Write([]byte("PWNED"), 0o644)
			// A real directory planted at the name is the one benign shape:
			// EEXIST on a genuine directory is an honest concurrent writer,
			// and the write must land inside the repository.
			if shape.name == "real_directory" {
				if err != nil {
					t.Fatalf("a genuinely concurrent mkdir must not break the write: %v", err)
				}
				data, readErr := os.ReadFile(filepath.Join(root, "created", "leaf.txt"))
				if readErr != nil || string(data) != "PWNED" {
					t.Fatalf("the write did not land inside the repository: %q (%v)", data, readErr)
				}
			} else if err == nil {
				t.Fatalf("Write reported success with a %s planted at a created component", shape.name)
			}
			assertUnchanged(t, outside, before)
		})
	}
}

// TestWriteRefusesALeafThatAppearedDuringTheWindow: the pin recorded that
// nothing was at the leaf, so the write is a creation. If something is there
// by the time the write runs, the pin's account of the world is stale and the
// write must fail closed rather than clobber whatever arrived.
func TestWriteRefusesALeafThatAppearedDuringTheWindow(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinned, err := pinWriteTarget(root, "dir/leaf.txt")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if pinned.targetExisted {
		t.Fatal("the leaf was pinned as existing")
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "leaf.txt"), []byte("someone else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pinned.Write([]byte("PAYLOAD"), 0o644); err == nil {
		t.Fatal("Write clobbered a file that appeared after policy evaluated the path")
	}
	data, err := os.ReadFile(filepath.Join(root, "dir", "leaf.txt"))
	if err != nil || string(data) != "someone else\n" {
		t.Fatalf("the bystander file was modified: %q (%v)", data, err)
	}
}

// TestWriteRefusesTailCreatedThenSwappedThenRecreated: the attacker lets the
// directory be created, replaces it with a link, and puts a decoy directory of
// the same name back at the end. Identity, not existence, is what is checked.
func TestWriteRefusesTailCreatedThenSwappedThenRecreated(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	before := treeSnapshot(t, outside)

	pinned, err := pinWriteTarget(root, "tail/leaf.txt")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}

	created := filepath.Join(root, "tail")
	if err := os.Mkdir(created, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(created); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, created); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(created); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(created, 0o755); err != nil {
		t.Fatal(err)
	}

	// The name now holds a real directory inside the repository, so the write
	// is allowed — but it must have landed inside, not outside.
	if err := pinned.Write([]byte("inside"), 0o644); err != nil {
		t.Fatalf("write into a re-created in-repository directory: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(created, "leaf.txt"))
	if readErr != nil || string(data) != "inside" {
		t.Fatalf("the write did not land inside the repository: %q (%v)", data, readErr)
	}
	assertUnchanged(t, outside, before)
}

// TestWriteSurvivesSwapRacingTheWriteItself runs the attacker concurrently
// with the write instead of before it. The outcome may legitimately be either
// a refusal or a write that landed inside the repository; what may never
// happen is a byte outside it.
func TestWriteSurvivesSwapRacingTheWriteItself(t *testing.T) {
	completed := 0
	for attempt := 0; attempt < 40; attempt++ {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "leaf.txt"), []byte("innocent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := treeSnapshot(t, outside)

		pinned, err := pinWriteTarget(root, "racy/leaf.txt")
		if err != nil {
			t.Fatalf("pin: %v", err)
		}

		target := filepath.Join(root, "racy")
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				os.RemoveAll(target)
				os.Symlink(outside, target)
				os.Remove(target)
				os.Mkdir(target, 0o755)
			}
		}()
		// Stand in for the approval delay: the attacker is already swapping
		// when the write begins.
		time.Sleep(time.Millisecond)
		writeErr := pinned.Write([]byte("PAYLOAD"), 0o644)
		close(stop)
		wg.Wait()

		assertUnchanged(t, outside, before)
		// The racing goroutine deletes and re-creates the directory, so a
		// completed write may leave nothing readable behind. The invariant
		// that matters is that no byte of it reached the outside tree, which
		// assertUnchanged has just proved; the counter keeps the test honest
		// about how much of the time it is exercising the refusal path alone.
		if writeErr == nil {
			completed++
		}
	}
	t.Logf("%d of 40 racing writes completed and %d refused; none reached the outside tree", completed, 40-completed)
}

// TestSpellingVariantsCannotSmuggleADifferentFile: this volume is case- and
// normalization-insensitive, so "Foo.txt", "foo.txt" and the NFD spelling of
// an NFC name are the same file. Identity checks must treat them as the same
// file (no spurious refusal) and containment must hold for every spelling.
func TestSpellingVariantsCannotSmuggleADifferentFile(t *testing.T) {
	insensitive := func(t *testing.T, dir, a, b string) bool {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, a), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		fiA, err := os.Stat(filepath.Join(dir, a))
		if err != nil {
			t.Fatal(err)
		}
		fiB, err := os.Stat(filepath.Join(dir, b))
		if err != nil {
			return false
		}
		return os.SameFile(fiA, fiB)
	}

	variants := []struct{ name, written, requested string }{
		{"case", "Foo.txt", "foo.txt"},
		{"nfc_nfd", "caf\u00e9.txt", "cafe\u0301.txt"},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			probe := t.TempDir()
			if !insensitive(t, probe, v.written, v.requested) {
				t.Skipf("this volume distinguishes %q from %q", v.written, v.requested)
			}

			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "dir", v.written), []byte("original\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			pinned, err := pinWriteTarget(root, "dir/"+v.requested)
			if err != nil {
				t.Fatalf("pin: %v", err)
			}
			if !pinned.targetExisted {
				t.Fatalf("the %s variant of an existing file was pinned as a creation", v.name)
			}
			if err := pinned.Write([]byte("rewritten\n"), 0o644); err != nil {
				t.Fatalf("write through the %s variant: %v", v.name, err)
			}
			data, err := os.ReadFile(filepath.Join(root, "dir", v.written))
			if err != nil || string(data) != "rewritten\n" {
				t.Fatalf("the %s variant wrote somewhere else: %q (%v)", v.name, data, err)
			}
		})
	}
}

// TestSpellingVariantOfACreatedDirectoryCannotEscape: the same insensitivity
// applied to a component the write creates. Requesting "DIR/leaf.txt" when the
// attacker plants a link at "dir" must still refuse.
func TestSpellingVariantOfACreatedDirectoryCannotEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	probeA, probeB := "Dir", "dir"
	if err := os.Mkdir(filepath.Join(root, probeA), 0o755); err != nil {
		t.Fatal(err)
	}
	fiA, err := os.Stat(filepath.Join(root, probeA))
	if err != nil {
		t.Fatal(err)
	}
	fiB, err := os.Stat(filepath.Join(root, probeB))
	if err != nil || !os.SameFile(fiA, fiB) {
		t.Skip("this volume is case-sensitive")
	}
	if err := os.Remove(filepath.Join(root, probeA)); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, outside)

	pinned, err := pinWriteTarget(root, "Dir/leaf.txt")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := pinned.Write([]byte("PWNED"), 0o644); err == nil {
		t.Fatal("Write reported success through a case-variant symlink")
	}
	assertUnchanged(t, outside, before)
}

// TestDeepCreationStillWorks: the fix must not have turned directory creation
// off. Nothing exists below the root; everything is created and the content
// lands where policy said.
func TestDeepCreationStillWorks(t *testing.T) {
	root := t.TempDir()
	pinned, err := pinWriteTarget(root, "x/y/z/file.txt")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := pinned.Write([]byte("deep\n"), 0o644); err != nil {
		t.Fatalf("deep create: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "x", "y", "z", "file.txt"))
	if err != nil || string(data) != "deep\n" {
		t.Fatalf("deep create landed wrong: %q (%v)", data, err)
	}
	got, err := ReadPinned(root, "x/y/z/file.txt", 0)
	if err != nil || string(got) != "deep\n" {
		t.Fatalf("read back: %q (%v)", got, err)
	}
}

// TestReadRefusesSwapOfAnUnpinnedComponent: Read walks the same chain. A path
// whose directory did not exist at pin time cannot be read at all — the pin
// says the file was not there — but the refusal must not be a resurrection of
// the chain either.
func TestReadRefusesSwapOfAnUnpinnedComponent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("OUTSIDE SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, outside)

	if _, err := pinWriteTarget(root, "gone/secret.txt"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "gone")); err != nil {
		t.Fatal(err)
	}
	data, err := ReadPinned(root, "gone/secret.txt", 0)
	if err == nil {
		t.Fatalf("read through a symlinked component that did not exist at pin time: %q", data)
	}
	assertUnchanged(t, outside, before)
}

// TestRemoveRefusesDirectoriesAndSwaps: RemovePinned must not delete a
// directory, and must not delete anything outside the repository.
func TestRemoveRefusesDirectoriesAndSwaps(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, outside)

	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemovePinned(root, "adir"); err == nil {
		t.Fatal("RemovePinned deleted a directory")
	}
	if _, err := os.Stat(filepath.Join(root, "adir")); err != nil {
		t.Fatalf("the directory was removed anyway: %v", err)
	}

	// A file that exists at pin time, swapped for a link before removal, is
	// removed as the link (unlink never follows) — never as its target.
	if err := os.WriteFile(filepath.Join(root, "doomed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemovePinned(root, "doomed.txt"); err != nil {
		t.Fatalf("removing a plain file: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "victim.txt"), filepath.Join(root, "doomed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := RemovePinned(root, "doomed.txt"); err == nil {
		t.Fatal("RemovePinned accepted a symlinked leaf")
	}
	assertUnchanged(t, outside, before)
}
