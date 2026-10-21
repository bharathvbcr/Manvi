// Package childenv owns the environment a child process of this harness
// starts with.
//
// The rule it enforces is one sentence: no name in credentials.ProviderEnv
// reaches a child. A host may put a model key and the workspace it bills into
// this process's environment for the harness's own adapter; nothing this
// process starts — git, a managed coding agent, the desktop broker, a verifier,
// a quick action — inherits them.
//
// It used to be each spawn site's own decision, and most sites decided by
// omission: os/exec reads a nil Env as "inherit everything", and git children
// were started with os.Environ() plus a few settings. So a hook, a clean or
// smudge filter, an fsmonitor or an alias in an untrusted repository ran with
// the operator's provider keys in its environment, and a managed Claude agent
// that inherited ANTHROPIC_API_KEY moved from its own login to API billing
// without anyone choosing that.
//
// Two shapes, because the children answer two different questions:
//
//   - Git is built from an allowlist. Git needs very little, and everything it
//     needs can be named. A name not on the list does not reach it, so a
//     credential invented tomorrow is withheld today.
//
//   - Inherit is this process's environment with ProviderEnv removed. It is for
//     children that are the operator's own tools acting for the operator — a
//     coding agent, gh, a verifier — whose needs cannot be enumerated: an
//     allowlist would break them in ways that only show up on someone else's
//     machine.
//
// The agent shell and MCP servers keep their own allowlists
// (devcouncil.childEnvAllowlist, mcp.baseEnvAllowlist), each checked against
// ProviderEnv by its package's tests.
package childenv

import (
	"os"
	"runtime"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
)

// Withheld names the variables no child of this process receives; names compare
// case-insensitively on Windows, as the OS does. It is
// credentials.ProviderEnv, re-exported under the name of what this package
// guarantees, so a host reading `manvi version --json` and a test reading the
// code see the same list.
func Withheld() []string { return credentials.ProviderEnv() }

// Capability is what this build tells a host that is deciding whether to start
// it with a provider credential in its environment: `manvi version --json`
// answers it before the process exists, and the serve `hello` answers it again
// from the running one. A host checks that every name it intends to inject is
// in WithheldProviderEnv (names compare case-insensitively on Windows), not
// only the boolean — a name this build does not
// know is a name it does not withhold.
//
// Absent is not false. A build that predates the guarantee omits both keys,
// and a host must read that as "this binary does not withhold", because it
// does not.
type Capability struct {
	WithholdsProviderCredentials bool     `json:"withholds_provider_credentials"`
	WithheldProviderEnv          []string `json:"withheld_provider_env"`
}

// Advertised is this build's Capability. The boolean is not a constant: a
// catalogue that named nothing would withhold nothing, and says so.
func Advertised() Capability {
	withheld := Withheld()
	return Capability{WithholdsProviderCredentials: len(withheld) > 0, WithheldProviderEnv: withheld}
}

// foldEnvNames is whether this OS compares environment variable names without
// regard to case. Windows does, so there `Anthropic_Api_Key` and
// `ANTHROPIC_API_KEY` are one variable and a child reads either spelling; unix
// does not. It is a variable rather than a constant so a test can exercise both
// behaviours on any host; the exported functions read it, the internal ones take
// it as an argument.
var foldEnvNames = runtime.GOOS == "windows"

// envKey is the form of an environment variable name that two names share
// exactly when this OS treats them as the same variable. Folding uses
// strings.ToUpper, as Windows' own name comparison upper-cases, rather than
// EqualFold: EqualFold would also join U+212A (Kelvin sign) to K, a name
// Windows keeps distinct. The cost of a fold wider than the OS's, such as the
// dotless i, is a withheld variable no real provider uses, which is the safe
// direction for a filter whose job is to withhold.
func envKey(name string, fold bool) string {
	if fold {
		return strings.ToUpper(name)
	}
	return name
}

// Key is the form of an environment variable name that two names share exactly
// when the host OS treats them as one variable: the name itself on unix, its
// upper-case on Windows. Every place that compares or de-duplicates names must
// go through it (or KeyFor), so the comparison has one owner.
func Key(name string) string { return envKey(name, foldEnvNames) }

// KeyFor is Key with the OS's behaviour chosen by the caller, so a test can
// exercise the Windows and unix comparisons on any host.
func KeyFor(fold bool) func(string) string {
	return func(name string) string { return envKey(name, fold) }
}

// entryName is the variable name of a "NAME=value" entry. A Windows drive
// variable such as `=C:=C:\work` has an empty name here and so matches no
// withheld or overriding name; it passes through untouched.
func entryName(entry string) string {
	name, _, _ := strings.Cut(entry, "=")
	return name
}

// WithoutProviderCredentials returns environ with every withheld name removed.
// Names are compared as this OS compares them, so on Windows every spelling of
// a withheld name is removed, including duplicates that differ only in case.
// The result is never nil, because os/exec reads a nil Env as "inherit
// everything" — an empty input would otherwise restore the leak this removes.
func WithoutProviderCredentials(environ []string) []string {
	return withoutProviderCredentials(environ, foldEnvNames)
}

func withoutProviderCredentials(environ []string, fold bool) []string {
	drop := map[string]bool{}
	for _, name := range Withheld() {
		drop[envKey(name, fold)] = true
	}
	env := make([]string, 0, len(environ))
	for _, entry := range environ {
		if drop[envKey(entryName(entry), fold)] {
			continue
		}
		env = append(env, entry)
	}
	return env
}

// Inherit is this process's environment without provider credentials.
func Inherit() []string { return WithoutProviderCredentials(os.Environ()) }

// Git is the environment for a git child: the names on gitAllowlist that are
// set here, then extra ("NAME=value"), whose entries replace an allowlisted
// value of the same name rather than appearing twice.
//
// The allowlist also keeps a parent's GIT_DIR, GIT_WORK_TREE or
// GIT_INDEX_FILE from steering this harness's own git at a repository other
// than the one its caller named; a caller that means to set one passes it in
// extra.
func Git(extra ...string) []string {
	return gitEnv(foldEnvNames, extra)
}

func gitEnv(fold bool, extra []string) []string {
	override := map[string]bool{}
	for _, entry := range extra {
		override[envKey(entryName(entry), fold)] = true
	}
	env := make([]string, 0, len(gitAllowlist)+len(extra))
	for _, name := range gitAllowlist {
		if override[envKey(name, fold)] {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env, extra...)
}

// gitAllowlist is the complete set of names a git child inherits. Adding one
// is a decision about what an untrusted repository's hooks and filters may
// read; nothing is matched by prefix, because a prefix rule written for one
// name covers the next credential that happens to share it.
var gitAllowlist = []string{
	// Where to find programs and configuration, and who is running them.
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "XDG_CONFIG_HOME",
	// Scratch space, text handling and the clock.
	"TMPDIR", "TMP", "TEMP", "LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "TZ", "TERM",
	// Which configuration git reads, and where its helpers live.
	"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_EXEC_PATH",
	// Identity for a commit the operator asked for.
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
	"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE", "EMAIL",
	// Transport and signing: the agent socket and prompts, not the keys.
	"SSH_AUTH_SOCK", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT",
	"GIT_ASKPASS", "SSH_ASKPASS", "DISPLAY", "GNUPGHOME", "GPG_TTY",
	// Proxies. A proxy URL can carry a password, which is the network's
	// credential rather than a model provider's, and git cannot reach a
	// remote behind one without it.
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	// The Windows spellings of the same few things. They are simply absent on
	// unix, where looking them up finds nothing, so this needs no build tag.
	"SystemRoot", "SystemDrive", "windir", "COMSPEC", "PATHEXT",
	"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "ProgramData",
}
