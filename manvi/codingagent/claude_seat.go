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

var errSeatFileSymlink = errors.New("is a symbolic link")

// readSmallRegularFile reads path only when it is a regular file of at most
// limit bytes. A name this package reads from the agent-writable checkout may
// be a FIFO or a device: opening a FIFO for reading blocks until a writer
// appears, and an unbounded file is unbounded memory. The open does not
// block, the kind is checked on the open descriptor (a swap between a stat and
// the open cannot slip through), and the read is capped.
func readSmallRegularFile(path string, limit int64) ([]byte, error) {
	return readRegular(path, limit, openNonBlocking)
}

// readCheckoutFile is readSmallRegularFile for a file the checkout controls
// (a marker, a worktree's .git, the gitdir and commondir it names): it never
// follows a symlink at the final component, because the checkout could point it
// at any file the operator can read and have that file's content treated as
// seat configuration or echoed in an error.
func readCheckoutFile(path string, limit int64) ([]byte, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errSeatFileSymlink
	}
	return readRegular(path, limit, openNoFollowNonBlocking)
}

func readRegular(path string, limit int64, open func(string) (*os.File, error)) ([]byte, error) {
	f, err := open(path)
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
//  1. `.claude-account` (on the checkout, an ancestor strictly below `$HOME`
//     or, outside `$HOME`, the checkout root only, or the main checkout of a
//     linked worktree whose link git's back-pointer proves) may only name a
//     valid seat identifier (`[A-Za-z0-9._-]+`); an empty or invalid marker
//     fails closed, a symlinked one is refused, and an error never repeats
//     marker content. A marker with no usable `$HOME` fails closed too.
//  2. Every launch drops the nested-session markers `CLAUDECODE` and
//     `CLAUDE_CODE_ENTRYPOINT`. Only when multi-seat routing is active
//     (`.claude-account` is present or `$HOME/.claude-accounts` exists) are
//     `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`,
//     `CLAUDE_CONFIG_DIR` and `CLAUDE_SECURESTORAGE_CONFIG_DIR` also stripped,
//     on both `personal` and named seats, matching `_claude_account_exec`. An
//     operator with no seats keeps their own `CLAUDE_CONFIG_DIR`,
//     `CLAUDE_SECURESTORAGE_CONFIG_DIR` and `CLAUDE_CODE_OAUTH_TOKEN`. The
//     API-key variables are withheld from every child by childenv before this
//     runs, seats or not.
//  3. A non-`personal` seat must resolve in `$HOME/.claude-accounts` to an
//     existing directory that is a different directory (by file identity, not
//     spelling) from `$HOME/.claude` and from every other registered seat, named
//     once, and sets both `CLAUDE_CONFIG_DIR` and
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
		// Without a usable home directory there is no ~/.claude-accounts to map
		// a seat from, so an operator with no seats gets the unrouted launch. A
		// checkout that pins a seat does not: launching it on the ambient account
		// is the one thing the marker exists to prevent.
		_, _, pinned, markerErr := findClaudeAccountMarker(cwd, "")
		if markerErr != nil {
			return envOverrides{}, markerErr
		}
		if pinned {
			return envOverrides{}, errors.New(".claude-account pins this checkout to a seat, but HOME is not a usable absolute path; refusing to launch on the default account")
		}
		return envOverrides{unsetKeys: nestedSessionMarkers}, nil
	}

	markerRoot, account, hasMarker, err := findClaudeAccountMarker(cwd, home)
	if err != nil {
		return envOverrides{}, err
	}
	if hasMarker && account == "" {
		return envOverrides{}, fmt.Errorf("empty .claude-account marker in %s", markerRoot)
	}
	if hasMarker && !validClaudeSeatName(account) {
		// The marker is checkout content: say what is wrong with it, never what it holds.
		return envOverrides{}, fmt.Errorf("invalid .claude-account in %s (first token is %d bytes and not a seat name)", markerRoot, len(account))
	}

	seats, hasAccountsFile, err := loadClaudeAccounts(home)
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

// markerSearchDirs lists the directories whose .claude-account may pin cwd,
// nearest first, compared by identity rather than spelling. Inside $HOME that is
// the checkout and every ancestor strictly below $HOME: a seat marker is the
// operator's own, kept in or above a project under their home. Outside $HOME
// (or with no usable home) it is the checkout root only, which is cwd or its
// nearest ancestor holding a .git: a marker in some shared parent directory is
// not something the operator placed for this checkout.
func markerSearchDirs(cwd, home string) []string {
	var homeInfo os.FileInfo
	if home != "" {
		homeInfo, _ = os.Stat(home)
	}
	var chain []string
	underHome := false
	for dir := filepath.Clean(cwd); ; {
		if homeInfo != nil {
			if info, err := os.Stat(dir); err == nil && os.SameFile(info, homeInfo) {
				underHome = true
				break
			}
		}
		chain = append(chain, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if underHome || len(chain) == 0 {
		return chain
	}
	for _, dir := range chain {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			if dir == chain[0] {
				break
			}
			return []string{chain[0], dir}
		}
	}
	return chain[:1]
}

func findClaudeAccountMarker(cwd, home string) (string, string, bool, error) {
	if root, acct, ok, err := searchClaudeAccountMarker(markerSearchDirs(cwd, home)); ok || err != nil {
		return root, acct, ok, err
	}
	if mainRoot := linkedWorktreeMainCheckout(cwd, home); mainRoot != "" {
		return searchClaudeAccountMarker(markerSearchDirs(mainRoot, home))
	}
	return "", "", false, nil
}

func searchClaudeAccountMarker(dirs []string) (string, string, bool, error) {
	for _, dir := range dirs {
		candidate := filepath.Join(dir, ".claude-account")
		info, statErr := os.Lstat(candidate)
		if statErr != nil || info.IsDir() {
			continue
		}
		raw, readErr := readCheckoutFile(candidate, 4096)
		switch {
		case errors.Is(readErr, errSeatFileSymlink):
			return dir, "", true, fmt.Errorf("%s is a symbolic link; a seat marker must be a regular file in the checkout", candidate)
		case errors.Is(readErr, errSeatFileTooLarge):
			return dir, "", true, errors.New(".claude-account exceeds 4 KiB")
		case readErr != nil:
			return dir, "", true, fmt.Errorf("read %s: %w", candidate, readErr)
		}
		fields := strings.Fields(strings.TrimSpace(string(raw)))
		if len(fields) == 0 {
			return dir, "", true, nil
		}
		return dir, fields[0], true, nil
	}
	return "", "", false, nil
}

// linkedWorktreeMainCheckout returns the main checkout of the linked worktree
// that holds cwd, or "" when cwd is not in one or the link does not prove
// itself. The worktree's .git file, and the gitdir and commondir it names, are
// written by the checkout, so none is believed on its own: the gitdir must be
// <common>/worktrees/<name> of the repository it claims, and git's back-pointer
// <gitdir>/gitdir must name this worktree's own .git file. A forged pair that
// points at another repository fails both.
func linkedWorktreeMainCheckout(cwd, home string) string {
	dotGit := ""
	for _, dir := range markerSearchDirs(cwd, home) {
		candidate := filepath.Join(dir, ".git")
		info, err := os.Lstat(candidate)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() || info.Size() > 4096 {
			return ""
		}
		dotGit = candidate
		break
	}
	if dotGit == "" {
		return ""
	}
	raw, err := readCheckoutFile(dotGit, 4096)
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
		gitDir = filepath.Join(filepath.Dir(dotGit), gitDir)
	}
	gitDir = filepath.Clean(gitDir)

	back, err := readCheckoutFile(filepath.Join(gitDir, "gitdir"), 4096)
	if err != nil {
		return ""
	}
	backPath := strings.TrimSpace(string(back))
	if !filepath.IsAbs(backPath) {
		backPath = filepath.Join(gitDir, backPath)
	}
	backInfo, backErr := os.Stat(backPath)
	ownInfo, ownErr := os.Stat(dotGit)
	if backErr != nil || ownErr != nil || !os.SameFile(backInfo, ownInfo) {
		return ""
	}

	commonDir := gitDir
	if cRaw, rErr := readCheckoutFile(filepath.Join(gitDir, "commondir"), 4096); rErr == nil {
		if rel := strings.TrimSpace(string(cRaw)); rel != "" {
			if filepath.IsAbs(rel) {
				commonDir = filepath.Clean(rel)
			} else {
				commonDir = filepath.Clean(filepath.Join(gitDir, rel))
			}
		}
	}
	if filepath.Base(filepath.Dir(gitDir)) != "worktrees" {
		return ""
	}
	adminParent, aErr := os.Stat(filepath.Dir(filepath.Dir(gitDir)))
	common, cErr := os.Stat(commonDir)
	if aErr != nil || cErr != nil || !os.SameFile(adminParent, common) {
		return ""
	}
	if resolved, rErr := filepath.EvalSymlinks(commonDir); rErr == nil {
		commonDir = resolved
	}
	if filepath.Base(commonDir) == ".git" {
		return filepath.Dir(commonDir)
	}
	return ""
}

func loadClaudeAccounts(home string) (map[string]string, bool, error) {
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

	// Directory identity is the file system's, not a spelling: a case variant or
	// a symlink of ~/.claude, or two seats naming one directory, is one directory.
	defaultInfo, _ := os.Stat(filepath.Join(home, ".claude"))
	type seatIdentity struct {
		name string
		info os.FileInfo
	}
	var seen []seatIdentity
	seats := make(map[string]string)
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
		if _, dup := seats[acct]; dup {
			return nil, true, fmt.Errorf("seat %q is defined twice in ~/.claude-accounts", acct)
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
		if defaultInfo != nil && os.SameFile(dInfo, defaultInfo) {
			return nil, true, fmt.Errorf("seat %q points at the default ~/.claude directory", acct)
		}
		for _, prev := range seen {
			if os.SameFile(dInfo, prev.info) {
				return nil, true, fmt.Errorf("seat %q shares config directory with %q", acct, prev.name)
			}
		}
		seen = append(seen, seatIdentity{acct, dInfo})
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
