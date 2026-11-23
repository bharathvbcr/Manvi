package tui

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func envFromMap(vals map[string]string) func(string) string {
	return func(k string) string {
		return vals[k]
	}
}

func writeExecScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { // #nosec G306 -- executable test fixture inside t.TempDir
		t.Fatal(err)
	}
}

func TestOSC52SequenceModesAndTruncation(t *testing.T) {
	t.Run("raw OSC 52", func(t *testing.T) {
		seq, trunc := osc52Sequence("hello world", envFromMap(nil))
		want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello world")) + "\x07"
		if trunc || seq != want {
			t.Fatalf("got (%q, %v), want (%q, false)", seq, trunc, want)
		}
	})

	t.Run("tmux DCS passthrough", func(t *testing.T) {
		seq, trunc := osc52Sequence("hello", envFromMap(map[string]string{
			"TMUX": "/tmp/tmux-1000/default,1,0",
			"STY":  "ignored-when-tmux-set",
		}))
		want := "\x1bPtmux;\x1b\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello")) + "\x07\x1b\\"
		if trunc || seq != want {
			t.Fatalf("got (%q, %v), want (%q, false)", seq, trunc, want)
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1bPtmux;"), "\x1b\\")
		for i := 0; i < len(inner); i++ {
			if inner[i] != '\x1b' {
				continue
			}
			if i+1 >= len(inner) || inner[i+1] != '\x1b' {
				t.Fatalf("unpaired ESC at index %d inside tmux DCS payload: %q", i, inner)
			}
			i++
		}
	})

	t.Run("screen DCS passthrough", func(t *testing.T) {
		seq, trunc := osc52Sequence("hello", envFromMap(map[string]string{
			"STY": "1234.pts-0.host",
		}))
		want := "\x1bP\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello")) + "\x07\x1b\\"
		if trunc || seq != want {
			t.Fatalf("got (%q, %v), want (%q, false)", seq, trunc, want)
		}
	})

	t.Run("screen DCS splits 96KB payload into bounded chunks", func(t *testing.T) {
		payload := strings.Repeat("S", osc52Limit)
		seq, trunc := osc52Sequence(payload, envFromMap(map[string]string{
			"STY": "1234.pts-0.host",
		}))
		if trunc {
			t.Fatal("exact 96KB payload should not report truncated")
		}
		var assembled strings.Builder
		rest := seq
		chunks := 0
		for len(rest) > 0 {
			if !strings.HasPrefix(rest, "\x1bP") {
				t.Fatalf("chunk %d missing DCS start: %.20q", chunks, rest)
			}
			rest = rest[2:]
			end := strings.Index(rest, "\x1b\\")
			if end < 0 {
				t.Fatalf("chunk %d missing DCS ST terminator", chunks)
			}
			chunk := rest[:end]
			if len(chunk) == 0 || len(chunk) > screenDCSChunkSize {
				t.Fatalf("chunk %d len = %d, want in [1, %d]", chunks, len(chunk), screenDCSChunkSize)
			}
			assembled.WriteString(chunk)
			rest = rest[end+2:]
			chunks++
		}
		if chunks < 2 {
			t.Fatalf("expected multi-chunk screen DCS output for 96KB, got %d chunks", chunks)
		}
		raw := assembled.String()
		if !strings.HasPrefix(raw, "\x1b]52;c;") || !strings.HasSuffix(raw, "\x07") {
			t.Fatalf("reassembled screen payload is not valid OSC 52: %.30q", raw)
		}
		b64 := strings.TrimSuffix(strings.TrimPrefix(raw, "\x1b]52;c;"), "\x07")
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("invalid base64 after reassembly: %v", err)
		}
		if string(decoded) != payload {
			t.Fatalf("decoded len = %d, want %d", len(decoded), len(payload))
		}
	})

	t.Run("96KB exact boundary is not truncated", func(t *testing.T) {
		exact := strings.Repeat("a", osc52Limit)
		seq, trunc := osc52Sequence(exact, envFromMap(nil))
		if trunc {
			t.Fatalf("exact %d byte payload reported truncated", osc52Limit)
		}
		want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(exact)) + "\x07"
		if seq != want {
			t.Fatalf("unexpected OSC 52 encoding length %d, want %d", len(seq), len(want))
		}
	})

	t.Run("over 96KB truncates to 96KB", func(t *testing.T) {
		over := strings.Repeat("b", osc52Limit+500)
		seq, trunc := osc52Sequence(over, envFromMap(nil))
		if !trunc {
			t.Fatal("expected truncated=true for payload over 96KB")
		}
		b64 := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\x07")
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("invalid base64: %v", err)
		}
		if len(decoded) != osc52Limit {
			t.Fatalf("decoded length = %d, want %d", len(decoded), osc52Limit)
		}
	})

	t.Run("multibyte UTF-8 truncation does not split rune", func(t *testing.T) {
		// 4-byte emoji repeated past 96KB; 98304 is divisible by 4, so prefix
		// with 1 ASCII byte so the limit lands mid-rune.
		payload := "x" + strings.Repeat("🚀", osc52Limit/4+10)
		seq, trunc := osc52Sequence(payload, envFromMap(nil))
		if !trunc {
			t.Fatal("expected truncated=true")
		}
		b64 := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\x07")
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("invalid base64: %v", err)
		}
		if len(decoded) > osc52Limit || len(decoded) < osc52Limit-3 {
			t.Fatalf("decoded len = %d, want within [limit-3, limit]", len(decoded))
		}
		if !strings.HasPrefix(payload, string(decoded)) {
			t.Fatal("truncated decoded output is not a valid prefix of input")
		}
	})
}

func TestClipboardSSHRemoteDetection(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "local", env: map[string]string{}, want: false},
		{name: "SSH_TTY", env: map[string]string{"SSH_TTY": "/dev/pts/1"}, want: true},
		{name: "SSH_CONNECTION", env: map[string]string{"SSH_CONNECTION": "10.0.0.1 1234 10.0.0.2 22"}, want: true},
		{name: "both", env: map[string]string{"SSH_TTY": "/dev/pts/1", "SSH_CONNECTION": "10.0.0.1 1 10.0.0.2 2"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRemoteSession(envFromMap(tc.env)); got != tc.want {
				t.Fatalf("isRemoteSession = %v, want %v", got, tc.want)
			}
			if tc.want {
				if _, err := writeNativeClipboard(context.Background(), "hi", envFromMap(tc.env)); err == nil {
					t.Fatal("writeNativeClipboard succeeded in remote SSH session")
				}
				if _, err := readNativeClipboard(context.Background(), envFromMap(tc.env)); err == nil {
					t.Fatal("readNativeClipboard succeeded in remote SSH session")
				}
			}
		})
	}
}

func TestClipboardResolveHardenedPATH(t *testing.T) {
	allTools := []string{"pbcopy", "pbpaste", "wl-copy", "wl-paste", "xclip", "xsel"}

	t.Run("disallowed binary names are rejected", func(t *testing.T) {
		safeBin := t.TempDir()
		writeExecScript(t, filepath.Join(safeBin, "sh"), "#!/bin/sh\nexit 0\n")
		for _, bad := range []string{"", "sh", "git", "/usr/bin/pbcopy", "../pbcopy", "pbcopy/sub"} {
			if got, err := resolveClipboardBinary(bad, envFromMap(map[string]string{"PATH": safeBin})); err == nil {
				t.Fatalf("resolveClipboardBinary(%q) unexpectedly succeeded: %q", bad, got)
			}
		}
	})

	t.Run("relative and empty PATH entries are rejected", func(t *testing.T) {
		workDir := t.TempDir()
		for _, sub := range []string{"", "bin"} {
			writeExecScript(t, filepath.Join(workDir, sub, "pbcopy"), "#!/bin/sh\nexit 0\n")
		}
		parentX := filepath.Join(filepath.Dir(workDir), "clip-rel-x")
		t.Cleanup(func() { _ = os.RemoveAll(parentX) })
		writeExecScript(t, filepath.Join(parentX, "pbcopy"), "#!/bin/sh\nexit 0\n")

		t.Chdir(workDir)
		for _, entry := range []string{".", "bin", "./bin", "../clip-rel-x", ""} {
			if got, err := resolveClipboardBinary("pbcopy", envFromMap(map[string]string{"PATH": entry})); err == nil {
				t.Fatalf("PATH=%q resolved %q, want rejection", entry, got)
			}
		}
	})

	t.Run("CWD and workspace PATH entries are rejected", func(t *testing.T) {
		repoRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o750); err != nil {
			t.Fatal(err)
		}
		subCwd := filepath.Join(repoRoot, "nested", "cwd")
		if err := os.MkdirAll(subCwd, 0o750); err != nil {
			t.Fatal(err)
		}
		repoBin := filepath.Join(repoRoot, "bin")
		cwdBin := filepath.Join(subCwd, "bin")
		projectOverride := t.TempDir()
		overrideBin := filepath.Join(projectOverride, "tools")

		for _, dir := range []string{subCwd, cwdBin, repoBin, overrideBin} {
			writeExecScript(t, filepath.Join(dir, "pbcopy"), "#!/bin/sh\nexit 0\n")
		}
		t.Chdir(subCwd)

		for _, untrusted := range []string{subCwd, cwdBin, repoBin} {
			if got, err := resolveClipboardBinary("pbcopy", envFromMap(map[string]string{"PATH": untrusted})); err == nil {
				t.Fatalf("untrusted PATH %q resolved %q, want rejection", untrusted, got)
			}
		}
		if got, err := resolveClipboardBinary("pbcopy", envFromMap(map[string]string{
			"PATH":               overrideBin,
			"MANVI_PROJECT_ROOT": projectOverride,
		})); err == nil {
			t.Fatalf("MANVI_PROJECT_ROOT PATH %q resolved %q, want rejection", overrideBin, got)
		}
	})

	t.Run("symlink from safe PATH dir into CWD is rejected", func(t *testing.T) {
		workDir := t.TempDir()
		planted := filepath.Join(workDir, "xclip")
		writeExecScript(t, planted, "#!/bin/sh\nexit 0\n")
		safeBin := t.TempDir()
		link := filepath.Join(safeBin, "xclip")
		if err := os.Symlink(planted, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Chdir(workDir)
		if got, err := resolveClipboardBinary("xclip", envFromMap(map[string]string{"PATH": safeBin})); err == nil {
			t.Fatalf("symlink into CWD resolved %q, want rejection", got)
		}
	})

	t.Run("non-executable and directory candidates are skipped", func(t *testing.T) {
		workDir := t.TempDir()
		badDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(badDir, "xsel"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(badDir, "wl-copy"), []byte("not-exec"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(workDir)
		if got, err := resolveClipboardBinary("xsel", envFromMap(map[string]string{"PATH": badDir})); err == nil {
			t.Fatalf("directory candidate resolved %q", got)
		}
		if got, err := resolveClipboardBinary("wl-copy", envFromMap(map[string]string{"PATH": badDir})); err == nil {
			t.Fatalf("non-executable candidate resolved %q", got)
		}
	})

	t.Run("skips untrusted prefix and resolves safe outside binary", func(t *testing.T) {
		workDir := t.TempDir()
		safeBin := t.TempDir()
		t.Chdir(workDir)

		for _, tool := range allTools {
			writeExecScript(t, filepath.Join(workDir, tool), "#!/bin/sh\nexit 1\n")
			writeExecScript(t, filepath.Join(safeBin, tool), "#!/bin/sh\nexit 0\n")

			pathEnv := strings.Join([]string{".", workDir, safeBin}, string(os.PathListSeparator))
			got, err := resolveClipboardBinary(tool, envFromMap(map[string]string{"PATH": pathEnv}))
			if err != nil {
				t.Fatalf("resolveClipboardBinary(%q) failed: %v", tool, err)
			}
			want, _ := filepath.EvalSymlinks(filepath.Join(safeBin, tool))
			if got != want {
				t.Fatalf("resolveClipboardBinary(%q) = %q, want %q", tool, got, want)
			}
		}
	})
}

func TestClipboardNativeExecutionIgnoresPlantedCWDAndWithholdsProviderEnv(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no native clipboard backend defined for GOOS=%s", runtime.GOOS)
	}

	workDir := t.TempDir()
	safeBin := t.TempDir()
	t.Chdir(workDir)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret-must-not-leak")

	writeTool := "pbcopy"
	readTool := "pbpaste"
	if runtime.GOOS == "linux" {
		writeTool = "xclip"
		readTool = "xclip"
	}

	plantedMarker := filepath.Join(workDir, "planted-ran")
	writeExecScript(t, filepath.Join(workDir, writeTool), "#!/bin/sh\ntouch '"+plantedMarker+"'\nexit 0\n")
	if readTool != writeTool {
		writeExecScript(t, filepath.Join(workDir, readTool), "#!/bin/sh\ntouch '"+plantedMarker+"'\nexit 0\n")
	}

	// Verify planted CWD-only PATH is never executed.
	if _, err := writeNativeClipboard(context.Background(), "hello", envFromMap(map[string]string{"PATH": "."})); err == nil {
		t.Fatal("writeNativeClipboard succeeded with PATH=.")
	}
	if _, err := os.Stat(plantedMarker); err == nil {
		t.Fatal("planted CWD binary executed via PATH=.")
	}

	// Install a safe mock in safeBin that records stdin and checks ANTHROPIC_API_KEY is absent.
	captured := filepath.Join(safeBin, "captured.txt")
	leaked := filepath.Join(safeBin, "leaked.txt")
	script := "#!/bin/sh\n" +
		"if [ -n \"$ANTHROPIC_API_KEY\" ]; then echo \"$ANTHROPIC_API_KEY\" > '" + leaked + "'; fi\n" +
		"if [ \"$1\" = \"-o\" ]; then printf 'from-clipboard'; else cat > '" + captured + "'; fi\n"
	writeExecScript(t, filepath.Join(safeBin, writeTool), script)
	if readTool != writeTool {
		writeExecScript(t, filepath.Join(safeBin, readTool), "#!/bin/sh\n"+
			"if [ -n \"$ANTHROPIC_API_KEY\" ]; then echo \"$ANTHROPIC_API_KEY\" > '"+leaked+"'; fi\n"+
			"printf 'from-clipboard'\n")
	}

	pathEnv := strings.Join([]string{".", workDir, safeBin}, string(os.PathListSeparator))
	backend, err := writeNativeClipboard(context.Background(), "copied text", envFromMap(map[string]string{"PATH": pathEnv}))
	if err != nil {
		t.Fatalf("writeNativeClipboard failed: %v", err)
	}
	if backend != writeTool {
		t.Fatalf("backend = %q, want %q", backend, writeTool)
	}
	gotBytes, err := os.ReadFile(captured)
	if err != nil || string(gotBytes) != "copied text" {
		t.Fatalf("captured = %q (err=%v), want %q", string(gotBytes), err, "copied text")
	}

	gotRead, err := readNativeClipboard(context.Background(), envFromMap(map[string]string{"PATH": pathEnv}))
	if err != nil || gotRead != "from-clipboard" {
		t.Fatalf("readNativeClipboard = (%q, %v), want (%q, nil)", gotRead, err, "from-clipboard")
	}
	if _, err := os.Stat(plantedMarker); err == nil {
		t.Fatal("planted CWD binary was executed")
	}
	if _, err := os.Stat(leaked); err == nil {
		t.Fatal("provider credential ANTHROPIC_API_KEY leaked to clipboard subprocess")
	}
}

func TestClipboardNativeReadSanitizesAndCapsSize(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no native clipboard backend defined for GOOS=%s", runtime.GOOS)
	}

	workDir := t.TempDir()
	safeBin := t.TempDir()
	t.Chdir(workDir)

	readTool := "pbpaste"
	if runtime.GOOS == "linux" {
		readTool = "xclip"
	}

	fixture := filepath.Join(safeBin, "clipboard.raw")
	rawBytes := append(
		[]byte("a\r\nb\rc\t\x00\x1b[31mred\x7f\xc2\x80\xc2\x9bZ\n"),
		bytesRepeat('x', maxPasteBytes+512)...,
	)
	if err := os.WriteFile(fixture, rawBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecScript(t, filepath.Join(safeBin, readTool), "#!/bin/sh\ncat '"+fixture+"'\n")

	got, err := readNativeClipboard(context.Background(), envFromMap(map[string]string{"PATH": safeBin}))
	if err != nil {
		t.Fatalf("readNativeClipboard failed: %v", err)
	}
	if len(got) > maxPasteBytes {
		t.Fatalf("readNativeClipboard returned %d bytes, want <= %d", len(got), maxPasteBytes)
	}
	wantPrefix := "a\nb\nc\t[31mredZ\n"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("sanitized prefix = %q, want %q", got[:min(len(got), len(wantPrefix)+10)], wantPrefix)
	}
	if strings.ContainsAny(got, "\r\x00\x1b\x7f") {
		t.Fatal("control characters survived native clipboard sanitization")
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
