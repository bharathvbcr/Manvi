package serve

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// runGit runs git in dir and reads at most limit bytes of stdout. The read
// is capped during the copy: a worktree holding an unignored dependency tree
// lists millions of paths, and the bound has to hold before the answer exists.
//
// This runs on the host, in a worktree the agent can write, so the repository
// is not trusted to name a program through the two channels the helper pins:
// core.hooksPath at the null device means no hook is found (a tracked .husky or
// .githooks is an ordinary writable path), and core.fsmonitor=false is the same
// for a monitor named in its config. Those are the only two the helper itself
// closes. Today's callers run diff, ls-files and rev-parse, which run no hook,
// and GIT_OPTIONAL_LOCKS=0 stops the index write a post-index-change hook needs.
//
// A diff driver (diff.external, diff.<drv>.textconv) is not pinned here; it is
// closed only because the callers pass --no-ext-diff --no-textconv, so a new
// diff caller must do the same.
//
// Everything else the repository's config can name runs if the subcommand
// reaches it: filter.<x>.clean and process (on any diff that compares content),
// core.sshCommand, core.editor and gpg.program. That is by decision (2026-11-07,
// the gitSpec decision block in devcouncil/verify.go): .git/config is the
// operator's protected write path, so these are the operator's configured
// commands, and the agent only routes content it already controls through them.
// The childenv allowlist keeps provider keys out of their environment. The same
// two settings are carried by bootstrap's git and, for the shell, by gitSpec.
func runGit(ctx context.Context, dir string, limit int, args ...string) (out []byte, truncated bool, err error) {
	// #nosec G204 -- the binary is the literal "git"; every caller places a
	// request-supplied revision behind --end-of-options or verifies it first.
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-pager", "-c", "core.quotepath=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = dir
	// Built, not inherited: a hook or filter in the repository runs with it.
	cmd.Env = childenv.Git("GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
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
