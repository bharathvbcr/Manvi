package codingagent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ClaudeBinaryVariable names the environment variable that selects the Claude
// executable. The serve host reads it; the refusal below names it so a person
// who hits the refusal is told what to change.
const ClaudeBinaryVariable = "MANVI_CLAUDE_BINARY"

// NotNativeExecutableError is the typed refusal for a configured provider
// program that is not a compiled executable.
//
// The managed lane holds the Claude CLI to the process this host started: the
// session's own report of its pid has to equal the pid that was spawned. A
// wrapper script (an account picker, an nvm or asdf shim) runs the real CLI as
// a child, so the CLI reports a pid the host never started and the run is
// refused only after it has begun. That check stays exactly as strict; this
// error is the early, explicit statement of the same rule, raised before any
// process exists.
type NotNativeExecutableError struct {
	// Path is the program as it was looked up; Resolved is where it led after
	// symlinks, and equals Path when it was not one.
	Path, Resolved string
	// Reason says what was found instead of a native executable.
	Reason string
}

func (e *NotNativeExecutableError) Error() string {
	where := e.Path
	if e.Resolved != "" && e.Resolved != e.Path {
		where = e.Path + " (resolved to " + e.Resolved + ")"
	}
	return fmt.Sprintf("managed Claude executable %s is not a native executable: %s. "+
		"A wrapper makes the CLI report a process this host did not start, which the host refuses; "+
		"set %s to the real Claude executable", where, e.Reason, ClaudeBinaryVariable)
}

// magicBytes is how much of the file is read: enough for every header below
// (a universal Mach-O needs eight) and nothing a hostile file can inflate.
const magicBytes = 8

// requireNativeExecutable reads at most magicBytes from the program and refuses
// anything that is not a Mach-O, ELF or PE image. The program is never run, and
// a script is not parsed: its interpreter line says nothing the host can trust
// about what the script will later start.
//
// It is a launch-time check, not a guarantee: the file can be replaced between
// this read and the spawn. The post-start process-identity check is the
// backstop for that, and for any wrapper this read cannot recognise.
func requireNativeExecutable(program string) error {
	resolved, err := filepath.EvalSymlinks(program)
	if err != nil {
		return fmt.Errorf("resolve managed Claude executable: %w", err)
	}
	refuse := func(reason string) error {
		return &NotNativeExecutableError{Path: program, Resolved: resolved, Reason: reason}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("inspect managed Claude executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return refuse("it is not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 && !executableWithoutMode() {
		return refuse("it is not marked executable")
	}
	file, err := openNonBlocking(resolved)
	if err != nil {
		return fmt.Errorf("open managed Claude executable: %w", err)
	}
	defer func() { _ = file.Close() }()
	// A path swapped for a FIFO or device between the stat and the open is
	// caught here rather than read from.
	if opened, err := file.Stat(); err != nil || !opened.Mode().IsRegular() {
		return refuse("it is not a regular file")
	}
	head := make([]byte, magicBytes)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read managed Claude executable: %w", err)
	}
	head = head[:n]
	if bytes.HasPrefix(head, []byte("#!")) {
		return refuse("it is a script (#! interpreter line)")
	}
	if !nativeImage(head) {
		return refuse("its first bytes are not a Mach-O, ELF or PE image")
	}
	return nil
}

// nativeImage recognises the headers of the executable formats the supported
// platforms load: ELF, PE ("MZ"), thin Mach-O in either byte order and either
// width, and a universal Mach-O.
func nativeImage(head []byte) bool {
	if len(head) >= 4 {
		switch binary.BigEndian.Uint32(head) {
		case 0x7f454c46, // ELF
			0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe: // Mach-O
			return true
		case 0xcafebabe, 0xcafebabf:
			// A universal Mach-O shares its magic with a Java class file. The
			// next word is the architecture count there and the class-file
			// version (45 and up) in a class, so a small count is the tell.
			if len(head) >= 8 {
				count := binary.BigEndian.Uint32(head[4:8])
				return count >= 1 && count < 45
			}
			return false
		}
	}
	return len(head) >= 2 && head[0] == 'M' && head[1] == 'Z'
}
