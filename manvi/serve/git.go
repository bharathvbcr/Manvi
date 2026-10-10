package serve

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// runGit runs git in dir and reads at most limit bytes of stdout. The read
// is capped during the copy: a worktree holding an unignored dependency tree
// lists millions of paths, and the bound has to hold before the answer exists.
func runGit(ctx context.Context, dir string, limit int, args ...string) (out []byte, truncated bool, err error) {
	// #nosec G204 -- the binary is the literal "git"; every caller places a
	// request-supplied revision behind --end-of-options or verifies it first.
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-pager", "-c", "core.quotepath=false", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, left: 4096}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	out, readErr := io.ReadAll(io.LimitReader(pipe, int64(limit)+1))
	if len(out) > limit {
		out, truncated = out[:limit], true
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, false, ctx.Err()
	case readErr != nil:
		return nil, false, readErr
	case waitErr != nil && !truncated:
		return nil, false, fmt.Errorf("git %s: %w: %s", args[0], waitErr, strings.TrimSpace(stderr.String()))
	}
	return out, truncated, nil
}

// limitedWriter keeps at most left bytes of what is written to it and
// discards the rest, reporting every write as complete so a chatty process
// is never failed by its own stderr. Errors from the underlying writer are
// returned.
type limitedWriter struct {
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.left > 0 {
		n := min(len(p), l.left)
		l.left -= n
		if _, err := l.w.Write(p[:n]); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
