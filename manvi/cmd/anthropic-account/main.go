// anthropic-account launches a command with one Anthropic organisation's API
// key in its environment.
//
// Prepaid credit lives in a Console organisation and cannot be pooled, so a
// machine with two funded organisations has two keys and must pick one per
// run. Every host that starts Manvi used to make that pick itself — a shell
// function, Blitztree, Vani — each reading one unnamed Keychain item and, in
// two of them, a workspace id that belonged to the other organisation. This is
// the one place the pick is made:
//
//	anthropic-account exec [--account NAME] [--repo DIR] [--no-fallback] -- CMD [ARG...]
//	anthropic-account which [--account NAME] [--repo DIR] [--json]
//	anthropic-account list [--json]
//	anthropic-account doctor
//
// It is the "inject at launch" half of Manvi's env-only credential policy:
// Manvi itself still reads its key only from the environment, and this binary
// is what puts it there. Keys stay in the login Keychain as
// anthropic-api-key-<name>; ~/.anthropic-accounts lists the names, the first
// being the default. exec replaces itself with CMD, so the key exists only in
// CMD's environment and is never printed.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"syscall"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// deps is everything run touches outside its arguments, so a test can drive
// a launch without a Keychain or replacing the test binary.
type deps struct {
	home     string
	cwd      string
	getenv   func(string) string
	environ  []string
	keychain keychain
	lookPath func(string) (string, error)
	execve   func(path string, argv, env []string) error
	stdout   io.Writer
	stderr   io.Writer
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "anthropic-account:", err)
		os.Exit(1)
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "anthropic-account:", err)
		os.Exit(1)
	}
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	os.Exit(run(os.Args[1:], deps{
		home:   home,
		cwd:    cwd,
		getenv: os.Getenv,
		// CMD's environment is built, not inherited: every provider credential
		// this process holds is dropped here, and childEnv adds back only the
		// chosen account's. A Gemini or xAI key in the caller's environment
		// does not ride along to CMD; hosts that route to those providers
		// start Manvi without this launcher.
		environ:  childenv.Inherit(),
		keychain: securityCLI{user: name, timeout: 2 * time.Minute},
		lookPath: exec.LookPath,
		execve:   syscall.Exec,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
	}))
}

const usage = `usage:
  anthropic-account exec [--account NAME] [--repo DIR] [--no-fallback] -- CMD [ARG...]
  anthropic-account which [--account NAME] [--repo DIR] [--json]
  anthropic-account list [--json]
  anthropic-account doctor`

func run(args []string, d deps) int {
	if len(args) == 0 {
		fmt.Fprintln(d.stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "exec":
		err = runExec(args[1:], d)
	case "which":
		err = runWhich(args[1:], d)
	case "list":
		err = runList(args[1:], d)
	case "doctor":
		err = runDoctor(args[1:], d)
	case "-h", "--help", "help":
		fmt.Fprintln(d.stdout, usage)
		return 0
	default:
		fmt.Fprintf(d.stderr, "anthropic-account: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
	var usageErr *usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &usageErr):
		fmt.Fprintf(d.stderr, "anthropic-account: %v\n%s\n", err, usage)
		return 2
	default:
		fmt.Fprintln(d.stderr, "anthropic-account:", err)
		return 1
	}
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// selector holds the flags exec and which share.
type selector struct {
	account string
	repo    string
}

func (s *selector) bind(fs *flag.FlagSet) {
	fs.StringVar(&s.account, "account", "", "account name from ~/.anthropic-accounts")
	fs.StringVar(&s.repo, "repo", "", "directory to look for a .anthropic-account marker from (default: the working directory)")
}

func (s *selector) choose(d deps, withFallback bool) (selection, string, error) {
	accounts, path, err := loadRegistry(d.home)
	if err != nil {
		return selection{}, path, err
	}
	dir := s.repo
	if dir == "" {
		dir = d.cwd
	}
	sel, err := choose(accounts, s.account, d.getenv("ANTHROPIC_ACCOUNT"), dir, d.home, withFallback)
	return sel, path, err
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return &usageError{msg: fmt.Sprintf("%s: %v", fs.Name(), err)}
	}
	return nil
}

func runExec(args []string, d deps) error {
	fs := newFlagSet("exec")
	var sel selector
	sel.bind(fs)
	noFallback := fs.Bool("no-fallback", false, "do not hand the child a second organisation's key")
	if err := parse(fs, args); err != nil {
		return err
	}
	argv := fs.Args()
	if len(argv) == 0 {
		return &usageError{msg: "exec: no command after --"}
	}
	names, err := anthropicEnvNames()
	if err != nil {
		return err
	}
	chosen, _, err := sel.choose(d, !*noFallback)
	if err != nil {
		return err
	}
	key, err := readKey(d.keychain, chosen.Account)
	if err != nil {
		return err
	}
	var fallbackKey string
	if chosen.Fallback != nil {
		// A missing second key costs the failover, not the run: the chosen
		// account is fully usable, so this warns loudly and carries on.
		// `anthropic-account doctor` is where it fails.
		fallbackKey, err = readKey(d.keychain, *chosen.Fallback)
		if err != nil {
			fmt.Fprintf(d.stderr, "anthropic-account: warning: no fallback key, continuing on %s alone: %v\n", chosen.Account.Name, err)
			fallbackKey = ""
		}
	}
	path, err := d.lookPath(argv[0])
	if err != nil {
		return fmt.Errorf("exec %s: %w", argv[0], err)
	}
	env := childEnv(d.environ, names, chosen.Account, key, chosen.Fallback, fallbackKey)
	if err := d.execve(path, argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}

type whichReport struct {
	Account  string   `json:"account"`
	Source   string   `json:"source"`
	Registry string   `json:"registry"`
	Fallback *account `json:"fallback"`
	// Workspace is reported for the chosen account only; it is not a secret.
	Workspace string `json:"workspace,omitempty"`
}

// runWhich answers "which organisation would this launch bill" without
// touching the Keychain, so a host can show it in its UI.
func runWhich(args []string, d deps) error {
	fs := newFlagSet("which")
	var sel selector
	sel.bind(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return &usageError{msg: "which: unexpected arguments"}
	}
	chosen, path, err := sel.choose(d, true)
	if err != nil {
		return err
	}
	report := whichReport{
		Account:   chosen.Account.Name,
		Source:    chosen.Source,
		Registry:  path,
		Fallback:  chosen.Fallback,
		Workspace: chosen.Account.Workspace,
	}
	if *asJSON {
		return json.NewEncoder(d.stdout).Encode(report)
	}
	fmt.Fprintf(d.stdout, "account    %s\nsource     %s\n", report.Account, report.Source)
	fmt.Fprintf(d.stdout, "workspace  %s\n", orNone(report.Workspace))
	if report.Fallback != nil {
		fmt.Fprintf(d.stdout, "fallback   %s\n", report.Fallback.Name)
	} else {
		fmt.Fprintf(d.stdout, "fallback   none\n")
	}
	return nil
}

func runList(args []string, d deps) error {
	fs := newFlagSet("list")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return &usageError{msg: "list: unexpected arguments"}
	}
	accounts, _, err := loadRegistry(d.home)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(d.stdout).Encode(accounts)
	}
	for i, a := range accounts {
		mark := ""
		if i == 0 {
			mark = "  (default)"
		}
		fmt.Fprintf(d.stdout, "%s  %s%s\n", a.Name, orNone(a.Workspace), mark)
	}
	return nil
}

// runDoctor checks every listed account has its Keychain item, without
// reading any secret. Unlike exec it fails on a missing second key, because
// this is where a half-configured failover should surface.
func runDoctor(args []string, d deps) error {
	if len(args) > 0 {
		return &usageError{msg: "doctor: unexpected arguments"}
	}
	accounts, path, err := loadRegistry(d.home)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.stdout, "registry  %s (%d accounts)\n", path, len(accounts))
	missing := 0
	for _, a := range accounts {
		service := keychainPrefix + a.Name
		ok, err := d.keychain.has(service)
		switch {
		case err != nil:
			missing++
			fmt.Fprintf(d.stdout, "FAIL  %s  could not check %s: %v\n", a.Name, service, err)
		case !ok:
			missing++
			fmt.Fprintf(d.stdout, "FAIL  %s  no Keychain item %s\n", a.Name, service)
		default:
			fmt.Fprintf(d.stdout, "ok    %s  %s\n", a.Name, service)
		}
	}
	if missing > 0 {
		return fmt.Errorf("%d of %d accounts have no usable Keychain item", missing, len(accounts))
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
