package codingagent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeBinaryVariable names the environment variable that selects the Claude
// executable. The serve host reads it; the refusal below names it so a person
// who hits the refusal is told what to change.
const ClaudeBinaryVariable = "MANVI_CLAUDE_BINARY"

// ClaudeWrapperError is the typed refusal for a configured Claude program that
// is not the CLI itself, or a launcher the kernel runs in place of it.
//
// The managed lane holds the Claude CLI to the process this host started: the
// session's own report of its pid has to equal the pid that was spawned. A
// script run directly keeps that pid (the kernel executes its interpreter in
// place), so an npm install's `#!/usr/bin/env node` launcher passes. A wrapper
// that runs the real CLI as a child (an account picker, an nvm or asdf shim)
// does not: the CLI reports a pid the host never started, and the run is
// refused only after it has begun. That check stays exactly as strict; this
// error states the same rule early, before any process exists.
type ClaudeWrapperError struct {
	// Path is the program as it was looked up; Resolved is where it led after
	// symlinks, and equals Path when it was not one.
	Path, Resolved string
	// Reason says what it is instead.
	Reason string
}

func (e *ClaudeWrapperError) Error() string {
	where := e.Path
	if e.Resolved != "" && e.Resolved != e.Path {
		where = e.Path + " (resolved to " + e.Resolved + ")"
	}
	return fmt.Sprintf("the configured claude at %s is %s; a wrapper that starts the CLI as a child process "+
		"fails Manvi's process-identity check. Set %s to the Claude Code executable "+
		"(for example the file under ~/.local/share/claude/versions/, or the npm cli.js-backed claude)",
		where, e.Reason, ClaudeBinaryVariable)
}

// headBytes is how much of the file is read: a universal Mach-O header needs
// eight, a `#!` line has to fit in all of it, and a hostile file can inflate
// neither.
const headBytes = 256

// Interpreters of the CLI's own runtimes: a script they run in place is the
// CLI. Shells are the wrappers this check exists for.
var (
	cliRuntimes = map[string]bool{"node": true, "bun": true, "deno": true}
	shells      = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true}
)

// requireLaunchableClaude reads at most headBytes from the program and accepts
// a Mach-O, ELF or PE image, or a `#!` script whose interpreter is one of the
// CLI's runtimes. A shell or any other interpreter is refused. The program is
// never run and the script body is never parsed: a script's interpreter line is
// the only part the kernel acts on.
//
// It is a launch-time check, not a guarantee: the file can be replaced between
// this read and the spawn, and a runtime launcher can still start a child. The
// post-start process-identity check is the backstop for both.
func requireLaunchableClaude(program string) error {
	resolved, err := filepath.EvalSymlinks(program)
	if err != nil {
		return fmt.Errorf("resolve managed Claude executable: %w", err)
	}
	refuse := func(reason string) error {
		return &ClaudeWrapperError{Path: program, Resolved: resolved, Reason: reason}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("inspect managed Claude executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return refuse("not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 && !executableWithoutMode() {
		return refuse("not marked executable")
	}
	file, err := openNonBlocking(resolved)
	if err != nil {
		return fmt.Errorf("open managed Claude executable: %w", err)
	}
	defer func() { _ = file.Close() }()
	// A path swapped for a FIFO or device between the stat and the open is
	// caught here rather than read from.
	if opened, err := file.Stat(); err != nil || !opened.Mode().IsRegular() {
		return refuse("not a regular file")
	}
	head := make([]byte, headBytes)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read managed Claude executable: %w", err)
	}
	head = head[:n]
	if nativeImage(head) {
		return nil
	}
	if !bytes.HasPrefix(head, []byte("#!")) {
		return refuse("neither a Mach-O, ELF or PE executable nor a script")
	}
	interpreter, ok := shebangInterpreter(head)
	switch {
	case !ok:
		return refuse("a script whose interpreter line Manvi cannot read")
	case cliRuntimes[interpreter]:
		return nil
	case shells[interpreter]:
		return refuse("a shell wrapper (interpreter " + interpreter + ")")
	default:
		return refuse("a script run by " + interpreter + ", a launch shape Manvi cannot verify")
	}
}

// shebangInterpreter names the program a `#!` line runs: the basename of the
// interpreter path, or for `env` the command it is asked to run, past its
// options (`-S`, `-i`, `-u NAME`) and NAME=value assignments. The line has to
// end inside the bytes read; a longer one is not parsed.
func shebangInterpreter(head []byte) (string, bool) {
	line, _, found := bytes.Cut(head[2:], []byte("\n"))
	if !found {
		return "", false
	}
	fields := strings.Fields(strings.TrimSuffix(string(line), "\r"))
	if len(fields) == 0 {
		return "", false
	}
	name := filepath.Base(fields[0])
	if name != "env" {
		return name, true
	}
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "-u" || f == "--unset" || f == "-C" || f == "--chdir":
			i++
		case strings.HasPrefix(f, "-") || strings.Contains(f, "="):
		default:
			return filepath.Base(f), true
		}
	}
	return "", false
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
