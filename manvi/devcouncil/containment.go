package devcouncil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// Values of Deps.ExecContainment, which is the operator's
// harness.exec_containment. The empty string is off.
const (
	ContainmentOff     = "off"
	ContainmentSecrets = "secrets"
)

// sandboxExecPath is where macOS ships sandbox-exec. It is a fixed absolute
// path, never looked up on PATH: the child environment's PATH is the
// repository's to influence, and a PATH hit would be a binary the agent can
// write standing in for the thing that is supposed to contain it.
const sandboxExecPath = "/usr/bin/sandbox-exec"

// containmentLookup reports the sandbox launcher this host has, or why it has
// none. It is a variable so a test can stand in for a host without one; the
// production answer is only ever the macOS one.
var containmentLookup = func() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("sandbox-exec exists only on macOS and this host is %s", runtime.GOOS)
	}
	info, err := os.Stat(sandboxExecPath)
	if err != nil {
		return "", fmt.Errorf("%s is missing: %w", sandboxExecPath, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable file", sandboxExecPath)
	}
	return sandboxExecPath, nil
}

// Bounds on what a profile may cost. A repository with more secret-named paths
// than this is refused rather than sampled: a profile that denies some of the
// secret files while the command reports contained is the failure this exists
// to prevent. maxProfileBytes keeps the profile, which travels as one argv
// element, well inside macOS's 1 MiB ARG_MAX.
const (
	maxContainmentWalk = 2_000_000
	maxProfileBytes    = 256 * 1024
	// subtreeProbe is a file name no pattern names. A directory under which
	// the probe matches a secret pattern has every file below it matching, so
	// the directory is denied as a subtree, which also covers files created in
	// it while the command runs.
	subtreeProbe = ".manvi-containment-probe"
)

// secretDenials are the real paths a contained command may not read.
type secretDenials struct {
	literals, subpaths []string
}

// containArgv wraps argv so it runs under a seatbelt profile that denies
// reading the repository's secret files, or says why it cannot. It never
// returns an uncontained argv alongside a nil error: a caller that gets an
// error must not run the command.
func containArgv(ctx context.Context, root string, m policy.Matcher, argv []string) ([]string, error) {
	launcher, err := containmentLookup()
	if err != nil {
		return nil, err
	}
	denials, err := findSecretDenials(ctx, root, m)
	if err != nil {
		return nil, err
	}
	profile, err := seatbeltProfile(denials)
	if err != nil {
		return nil, err
	}
	return append([]string{launcher, "-p", profile}, argv...), nil
}

// findSecretDenials enumerates, under root, what policy.SecretPathPatterns
// names. The pattern table and the matcher are the policy package's own, read
// through the matcher the gate uses; nothing here restates either. Paths are
// resolved to their real location because seatbelt matches on the resolved
// path: a deny on a symlink's own name does not stop a read through the link's
// target, and a deny on the target does stop a read through any link.
func findSecretDenials(ctx context.Context, root string, m policy.Matcher) (secretDenials, error) {
	if m == nil {
		m = policy.GoMatcher
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return secretDenials{}, fmt.Errorf("resolving %s: %w", root, err)
	}
	literals, subpaths := map[string]bool{}, map[string]bool{}
	matches := func(rel string) (bool, error) { return m.MatchAnyFold(policy.SecretPathPatterns, rel) }
	deny := func(real string, dir bool) {
		if dir {
			subpaths[real] = true
		} else {
			literals[real] = true
		}
	}

	seen := 0
	walkErr := filepath.WalkDir(realRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", path, err)
		}
		if seen++; seen > maxContainmentWalk {
			return fmt.Errorf("more than %d entries under %s", maxContainmentWalk, realRoot)
		}
		if seen%1024 == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		rel, rerr := filepath.Rel(realRoot, path)
		if rerr != nil || rel == "." {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			hit, merr := matches(rel)
			if merr != nil {
				return merr
			}
			if !hit {
				return nil
			}
			target, lerr := filepath.EvalSymlinks(path)
			if lerr != nil {
				// A dangling link: nothing to read now. The link's own name is
				// denied so a target created mid-command is not read through it.
				deny(path, false)
				return nil //nolint:nilerr // a dangling link has nothing to resolve; it is denied by name above
			}
			info, serr := os.Stat(target)
			if serr != nil {
				return serr
			}
			deny(target, info.IsDir())
		case d.IsDir():
			if rel == ".git/objects" {
				// Packed history is not a working-tree secret file, and the
				// directory is the bulk of a large repository's entries.
				return fs.SkipDir
			}
			hit, merr := matches(rel + "/" + subtreeProbe)
			if merr != nil {
				return merr
			}
			if hit {
				deny(path, true)
				return fs.SkipDir
			}
		default:
			hit, merr := matches(rel)
			if merr != nil {
				return merr
			}
			if hit {
				deny(path, false)
			}
		}
		return nil
	})
	if walkErr != nil {
		return secretDenials{}, fmt.Errorf("enumerating secret files under %s: %w", root, walkErr)
	}
	return secretDenials{literals: sortedPaths(literals), subpaths: sortedPaths(subpaths)}, nil
}

func sortedPaths(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seatbeltProfile renders denials as an SBPL profile: everything allowed,
// reading the named paths denied. Without a rule list the profile is just the
// allow, since an empty deny form does not parse.
func seatbeltProfile(d secretDenials) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	if len(d.literals)+len(d.subpaths) > 0 {
		b.WriteString("(deny file-read*")
		for _, group := range []struct {
			filter string
			paths  []string
		}{{"literal", d.literals}, {"subpath", d.subpaths}} {
			for _, p := range group.paths {
				quoted, err := sbplString(p)
				if err != nil {
					return "", err
				}
				b.WriteString("\n  (" + group.filter + " " + quoted + ")")
			}
		}
		b.WriteString(")\n")
	}
	if b.Len() > maxProfileBytes {
		return "", fmt.Errorf("the containment profile is %d bytes for %d secret paths, over the %d byte bound",
			b.Len(), len(d.literals)+len(d.subpaths), maxProfileBytes)
	}
	return b.String(), nil
}

// sbplString renders s as an SBPL string literal. Quote and backslash are
// escaped, and newline, return and tab take their named escapes. A path that
// is not valid UTF-8, or holds another control character, has no safe literal
// and is an error: guessing would deny a different file than the one named.
func sbplString(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("path %q is not valid UTF-8, so it cannot be written into a containment profile", s)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			return "", fmt.Errorf("path %q holds a control character, so it cannot be written into a containment profile", s)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// containedShellArgv is the argv exec_command runs for command under mode.
// Anything but off or secrets is refused, as is a host that cannot contain:
// the command is not run, and the message says containment was unavailable.
func (r *Registry) containedShellArgv(ctx context.Context, command string) ([]string, error) {
	argv := []string{"sh", "-c", command}
	switch r.deps.ExecContainment {
	case "", ContainmentOff:
		return argv, nil
	case ContainmentSecrets:
		wrapped, err := containArgv(ctx, r.deps.Root, r.matcher(), argv)
		if err != nil {
			return nil, fmt.Errorf("secret-file containment is on (harness.exec_containment=secrets) but unavailable: %w; "+
				"the command was not run", err)
		}
		return wrapped, nil
	}
	return nil, errors.New("harness.exec_containment has an unknown value " + r.deps.ExecContainment + "; the command was not run")
}
