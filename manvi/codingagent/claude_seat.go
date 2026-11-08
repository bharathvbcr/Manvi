package codingagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxClaudeIdentityFileBytes = 16 * 1024 * 1024

var (
	errSeatFileNotRegular = errors.New("not a regular file")
	errSeatFileTooLarge   = errors.New("file exceeds its size bound")
)

// readSmallRegularFile reads path only when it is a regular file of at most
// limit bytes. The checkout is agent-writable, so a name this package reads
// there may be a FIFO or a device: opening a FIFO for reading blocks until a
// writer appears, and an unbounded file is unbounded memory. The open does not
// block, the kind is checked on the open descriptor (a swap between a stat and
// the open cannot slip through), and the read is capped.
func readSmallRegularFile(path string, limit int64) ([]byte, error) {
	f, err := openNonBlocking(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errSeatFileNotRegular
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errSeatFileTooLarge
	}
	return raw, nil
}

// claudeSeatOverrides resolves multi-account `.claude-account` seat markers
// against `$HOME/.claude-accounts` when Manvi launches a native Claude Code
// binary directly rather than through an operator's shell seat wrapper.
//
// Invariants enforced before any child process starts:
//  1. `.claude-account` (on the checkout, an ancestor below `$HOME`, or a
//     linked git worktree's common-dir parent checkout) may only name a valid
//     seat identifier (`[A-Za-z0-9._-]+`); an empty or invalid marker fails
//     closed.
//  2. Every launch drops the nested-session markers `CLAUDECODE` and
//     `CLAUDE_CODE_ENTRYPOINT`. Only when multi-seat routing is active
//     (`.claude-account` is present or `$HOME/.claude-accounts` exists) are
//     `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`,
//     `CLAUDE_CONFIG_DIR` and `CLAUDE_SECURESTORAGE_CONFIG_DIR` also stripped,
//     on both `personal` and named seats, matching `_claude_account_exec`. An
//     operator with no seats who authenticates managed Claude by a config
//     directory or token of their own keeps that launch exactly as it was.
//  3. A non-`personal` seat must resolve in `$HOME/.claude-accounts` to an
//     existing directory distinct from `$HOME/.claude` and distinct from all
//     other registered seats, and sets both `CLAUDE_CONFIG_DIR` and
//     `CLAUDE_SECURESTORAGE_CONFIG_DIR` to that directory.
//  4. When a checkout resolves to `personal` (unpinned or explicit `personal`)
//     and `$HOME/.claude-accounts` contains at least one named seat whose
//     `<seatDir>/.claude.json` has a non-empty `oauthAccount.emailAddress`,
//     `<home>/.claude.json` is checked: malformed JSON or a case-insensitive
//     email match against any named seat fails closed.
func claudeSeatOverrides(cwd string) (envOverrides, error) {
	nestedSessionMarkers := []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}

	home, err := os.UserHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(home) {
		// No usable home directory means no seats to route: the launch is the
		// unrouted one, not a failure.
		return envOverrides{unsetKeys: nestedSessionMarkers}, nil //nolint:nilerr // see above
	}
	resolvedHome := home
	if realHome, symErr := filepath.EvalSymlinks(home); symErr == nil {
		resolvedHome = realHome
	}

	markerRoot, account, hasMarker, err := findClaudeAccountMarker(cwd, home, resolvedHome)
	if err != nil {
		return envOverrides{}, err
	}
	if hasMarker && account == "" {
		return envOverrides{}, fmt.Errorf("empty .claude-account marker in %s", markerRoot)
	}
	if hasMarker && !validClaudeSeatName(account) {
		return envOverrides{}, fmt.Errorf("invalid .claude-account seat name %q", account)
	}

	seats, hasAccountsFile, err := loadClaudeAccounts(home, resolvedHome)
	if err != nil {
		return envOverrides{}, err
	}

	if !hasMarker && !hasAccountsFile {
		return envOverrides{unsetKeys: nestedSessionMarkers}, nil
	}
	unset := append(append([]string(nil), nestedSessionMarkers...),
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CONFIG_DIR",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR",
	)

	if hasMarker && account != "personal" {
		if !hasAccountsFile {
			return envOverrides{}, fmt.Errorf("pinned .claude-account %q requires ~/.claude-accounts", account)
		}
		seatDir, ok := seats[account]
		if !ok || seatDir == "" {
			return envOverrides{}, fmt.Errorf("unknown .claude-account %q in ~/.claude-accounts", account)
		}
		return envOverrides{
			unsetKeys: unset,
			set: []string{
				"CLAUDE_CONFIG_DIR=" + seatDir,
				"CLAUDE_SECURESTORAGE_CONFIG_DIR=" + seatDir,
			},
		}, nil
	}

	if hasAccountsFile && len(seats) > 0 {
		if err := verifyDefaultLoginNotPinnedSeat(home, seats); err != nil {
			return envOverrides{}, err
		}
	}

	return envOverrides{unsetKeys: unset}, nil
}

type seatLoginEmail struct {
	seat  string
	email string
}

func readClaudeConfigEmail(path string) (email string, exists bool, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, statErr
	}
	if info.IsDir() {
		return "", true, errors.New("path is a directory, expected JSON file")
	}
	raw, readErr := readSmallRegularFile(path, maxClaudeIdentityFileBytes)
	if errors.Is(readErr, errSeatFileTooLarge) {
		return "", true, errors.New("file exceeds 16 MiB bound")
	}
	if readErr != nil {
		return "", true, readErr
	}
	var payload struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
		return "", true, jsonErr
	}
	return strings.TrimSpace(payload.OAuthAccount.EmailAddress), true, nil
}

func verifyDefaultLoginNotPinnedSeat(home string, seats map[string]string) error {
	names := make([]string, 0, len(seats))
	for acct := range seats {
		names = append(names, acct)
	}
	sort.Strings(names)

	var namedEmails []seatLoginEmail
	for _, acct := range names {
		seatJSON := filepath.Join(seats[acct], ".claude.json")
		email, exists, err := readClaudeConfigEmail(seatJSON)
		if err != nil {
			return fmt.Errorf(
				"cannot read login identity for Claude seat %q (%s): %w; fix the seat config or pin this repository via .claude-account",
				acct,
				seatJSON,
				err,
			)
		}
		if exists && email != "" {
			namedEmails = append(namedEmails, seatLoginEmail{seat: acct, email: email})
		}
	}
	if len(namedEmails) == 0 {
		return nil
	}

	defaultJSON := filepath.Join(home, ".claude.json")
	defaultEmail, exists, err := readClaudeConfigEmail(defaultJSON)
	if err != nil {
		return fmt.Errorf(
			"cannot verify default ~/.claude.json against pinned seats (%w); fix ~/.claude.json, create .claude-account to pin a seat, or re-login default claude",
			err,
		)
	}
	if !exists || defaultEmail == "" {
		return nil
	}
	for _, entry := range namedEmails {
		if strings.EqualFold(defaultEmail, entry.email) {
			return fmt.Errorf(
				"default ~/.claude.json is signed into %s, which matches pinned seat %q; create .claude-account to pin this repo or re-login default claude to a separate account",
				defaultEmail,
				entry.seat,
			)
		}
	}
	return nil
}

func findClaudeAccountMarker(cwd, home, resolvedHome string) (string, string, bool, error) {
	if root, acct, ok, err := walkClaudeAccountAncestors(cwd, home, resolvedHome); ok || err != nil {
		return root, acct, ok, err
	}
	if mainRoot := linkedWorktreeMainCheckout(cwd, home, resolvedHome); mainRoot != "" {
		return walkClaudeAccountAncestors(mainRoot, home, resolvedHome)
	}
	return "", "", false, nil
}

func walkClaudeAccountAncestors(start, home, resolvedHome string) (string, string, bool, error) {
	dir := filepath.Clean(start)
	for dir != "/" && dir != "." && dir != home && dir != resolvedHome {
		candidate := filepath.Join(dir, ".claude-account")
		info, statErr := os.Stat(candidate)
		if statErr == nil && !info.IsDir() {
			raw, readErr := readSmallRegularFile(candidate, 4096)
			if errors.Is(readErr, errSeatFileTooLarge) {
				return dir, "", true, errors.New(".claude-account exceeds 4 KiB")
			}
			if readErr != nil {
				return dir, "", true, fmt.Errorf("read %s: %w", candidate, readErr)
			}
			trimmed := strings.TrimSpace(string(raw))
			if trimmed == "" {
				return dir, "", true, nil
			}
			fields := strings.Fields(trimmed)
			return dir, fields[0], true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", "", false, nil
}

func linkedWorktreeMainCheckout(cwd, home, resolvedHome string) string {
	dir := filepath.Clean(cwd)
	worktreeRoot := ""
	dotGit := ""
	for dir != "/" && dir != "." && dir != home && dir != resolvedHome {
		candidate := filepath.Join(dir, ".git")
		info, err := os.Stat(candidate)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() > 4096 {
				return ""
			}
			worktreeRoot = dir
			dotGit = candidate
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if dotGit == "" {
		return ""
	}
	raw, err := readSmallRegularFile(dotGit, 4096)
	if err != nil {
		return ""
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	gitDirText, ok := strings.CutPrefix(strings.TrimSpace(firstLine), "gitdir:")
	if !ok {
		return ""
	}
	gitDir := strings.TrimSpace(gitDirText)
	if gitDir == "" {
		return ""
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreeRoot, gitDir)
	}
	gitDir = filepath.Clean(gitDir)

	commonDir := gitDir
	commonFile := filepath.Join(gitDir, "commondir")
	if cRaw, rErr := readSmallRegularFile(commonFile, 4096); rErr == nil {
		rel := strings.TrimSpace(string(cRaw))
		if rel != "" {
			if filepath.IsAbs(rel) {
				commonDir = filepath.Clean(rel)
			} else {
				commonDir = filepath.Clean(filepath.Join(gitDir, rel))
			}
		}
	}
	if resolved, rErr := filepath.EvalSymlinks(commonDir); rErr == nil {
		commonDir = resolved
	}
	if filepath.Base(commonDir) == ".git" {
		return filepath.Dir(commonDir)
	}
	return ""
}

func loadClaudeAccounts(home, resolvedHome string) (map[string]string, bool, error) {
	path := filepath.Join(home, ".claude-accounts")
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat ~/.claude-accounts: %w", err)
	}
	if info.IsDir() {
		return nil, true, errors.New("invalid ~/.claude-accounts file")
	}
	raw, err := readSmallRegularFile(path, 65536)
	if errors.Is(err, errSeatFileNotRegular) || errors.Is(err, errSeatFileTooLarge) {
		return nil, true, errors.New("invalid ~/.claude-accounts file")
	}
	if err != nil {
		return nil, true, fmt.Errorf("read ~/.claude-accounts: %w", err)
	}

	defaultDir := filepath.Clean(filepath.Join(home, ".claude"))
	resolvedDefault := defaultDir
	if r, rErr := filepath.EvalSymlinks(defaultDir); rErr == nil {
		resolvedDefault = r
	} else if resolvedHome != home {
		resolvedDefault = filepath.Clean(filepath.Join(resolvedHome, ".claude"))
	}

	seats := make(map[string]string)
	byDir := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.IndexAny(trimmed, " \t")
		if idx <= 0 {
			continue
		}
		acct := trimmed[:idx]
		if acct == "personal" {
			continue
		}
		if !validClaudeSeatName(acct) {
			return nil, true, fmt.Errorf("invalid seat name %q in ~/.claude-accounts", acct)
		}
		dirText := strings.TrimSpace(trimmed[idx:])
		dirText = strings.TrimRight(dirText, "/")
		if dirText == "~" {
			dirText = home
		} else if rest, ok := strings.CutPrefix(dirText, "~/"); ok {
			dirText = filepath.Join(home, rest)
		}
		if !filepath.IsAbs(dirText) {
			return nil, true, fmt.Errorf("seat %q in ~/.claude-accounts must use an absolute path", acct)
		}
		dInfo, dErr := os.Stat(dirText)
		if dErr != nil || !dInfo.IsDir() {
			return nil, true, fmt.Errorf("seat %q config directory %s does not exist", acct, dirText)
		}
		resolvedDir, rErr := filepath.EvalSymlinks(dirText)
		if rErr != nil {
			return nil, true, fmt.Errorf("resolve seat %q config directory: %w", acct, rErr)
		}
		if resolvedDir == defaultDir || resolvedDir == resolvedDefault {
			return nil, true, fmt.Errorf("seat %q points at the default ~/.claude directory", acct)
		}
		if prev, exists := byDir[resolvedDir]; exists && prev != acct {
			return nil, true, fmt.Errorf("seat %q shares config directory with %q", acct, prev)
		}
		byDir[resolvedDir] = acct
		seats[acct] = resolvedDir
	}
	return seats, true, nil
}

func validClaudeSeatName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}
