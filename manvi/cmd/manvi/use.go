package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/local"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// manvi use chooses, once for the whole machine, whether Manvi runs on the
// cloud (Claude through the Anthropic API) or on a local model server.
//
// The choice is llm.provider.default in the operator's user config, the layer
// every invocation reads whichever program started it — a shell, GitPulse, a
// script. So a host that starts Manvi carries no copy of the choice, and
// switching is one command rather than one per app. The environment still wins
// over it, as it wins over every setting, and the status says so rather than
// letting a saved choice look applied.
//
// Credentials are not this command's business. It reads and writes no key, no
// account and no Keychain item; the cloud is billed exactly as before (the host
// launches manvi under the key it already holds), and the status carries no
// credential state — the host checks that itself.

const useUsage = `usage:
  manvi use                      Show which one Manvi runs on, and what each mode would use
  manvi use cloud [--model ID]   Run on Claude through the Anthropic API; --model sets the main tier
  manvi use local [--base-url URL] [--model ID]
                                 Run on a local model server ('manvi local' finds them)
  manvi use --json               The same status as JSON, for a host`

// Modes as the operator names them, and the provider each one is.
const (
	modeCloud = "cloud"
	modeLocal = "local"
	modeOther = "other"
)

func modeOf(provider string) string {
	switch provider {
	case anthropic.Name:
		return modeCloud
	case local.Name:
		return modeLocal
	}
	return modeOther
}

// useStatus is what `manvi use --json` answers. It is a host contract: GitPulse
// reads it, so fields are added, never renamed or removed.
type useStatus struct {
	Mode         string `json:"mode"`
	Provider     string `json:"provider"`
	ProviderFrom string `json:"provider_from"`
	// UserConfig is the file the choice is saved in, or empty when this process
	// has no user config location.
	UserConfig string `json:"user_config"`
	// ShadowedBy names every environment variable set in this process that
	// overrides a saved value this mode reads.
	ShadowedBy []string `json:"shadowed_by"`
	// Model is the model this mode runs, and ModelFrom the layer that supplied
	// it: default, config, env or flag. An empty local Model is not an error:
	// the run asks the server what it serves.
	Model     string   `json:"model"`
	ModelFrom string   `json:"model_from"`
	Local     useLocal `json:"local"`
	Cloud     useCloud `json:"cloud"`
	Warnings  []string `json:"warnings"`
}

type useLocal struct {
	BaseURL     string `json:"base_url"`
	BaseURLFrom string `json:"base_url_from"`
	// Model is llm.local.model alone. The model a local run sends is
	// useStatus.Model, which MANVI_MODEL can override.
	Model     string `json:"model"`
	ModelFrom string `json:"model_from"`
}

type useCloud struct {
	Model     string `json:"model"`
	ModelFrom string `json:"model_from"`
}

// maxLocalModelLen bounds a local model id; real ones are far shorter.
const maxLocalModelLen = 256

// runUse is `manvi use`. reg is the registry main already loaded, rebuild loads
// it again after a write so the status printed is what the next run will see.
func runUse(out io.Writer, reg *flags.Registry, args []string, rebuild func() (*flags.Registry, error)) error {
	fs := flag.NewFlagSet("use", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the status as JSON")
	model := fs.String("model", "", "the model: the main tier for cloud, llm.local.model for local")
	baseURL := fs.String("base-url", "", "the local server's base URL (local only)")
	// The mode may come before or after the flags.
	var mode string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		mode, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("manvi use: %w\n\n%s", err, useUsage)
	}
	if fs.NArg() > 0 {
		if mode != "" || fs.NArg() > 1 {
			return fmt.Errorf("manvi use: unexpected %q\n\n%s", strings.Join(fs.Args(), " "), useUsage)
		}
		mode = fs.Arg(0)
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if mode == "help" {
		_, err := fmt.Fprintln(out, useUsage)
		return err
	}
	if mode != "" && mode != modeCloud && mode != modeLocal {
		return fmt.Errorf("manvi use: %q is not a mode; use 'cloud' or 'local'\n\n%s", mode, useUsage)
	}
	if (given["model"] || given["base-url"]) && mode == "" {
		return fmt.Errorf("manvi use: --model and --base-url go with 'cloud' or 'local'\n\n%s", useUsage)
	}
	if given["base-url"] && mode != modeLocal {
		return fmt.Errorf("manvi use: --base-url goes with 'manvi use local'\n\n%s", useUsage)
	}

	if mode != "" {
		pairs, err := useChange(mode, *model, *baseURL, given)
		if err != nil {
			return fmt.Errorf("manvi use %s: %w", mode, err)
		}
		path, err := userConfigPath()
		if err != nil {
			return fmt.Errorf("manvi use: %w", err)
		}
		if err := refuseUserConfigInside(path, projectRoot()); err != nil {
			return fmt.Errorf("manvi use: %w", err)
		}
		if err := setUserConfigValues(path, pairs); err != nil {
			return fmt.Errorf("manvi use: %w", err)
		}
		if reg, err = rebuild(); err != nil {
			return fmt.Errorf("manvi use: saved, but the settings no longer load: %w", err)
		}
	}

	status := useStatusFor(reg)
	if *asJSON {
		return json.NewEncoder(out).Encode(status)
	}
	return printUseStatus(out, status)
}

type configPair struct{ key, value string }

// useChange validates the request and names every key it writes. Nothing has
// been written when it returns an error.
func useChange(mode, model, baseURL string, given map[string]bool) ([]configPair, error) {
	provider := anthropic.Name
	if mode == modeLocal {
		provider = local.Name
	}
	pairs := []configPair{{flags.LLMDefaultProvider, provider}}
	if given["model"] {
		model = strings.TrimSpace(model)
		if mode == modeCloud {
			if _, ok := anthropic.Capability(model); !ok {
				return nil, fmt.Errorf("%q is not in the Anthropic catalogue (it serves: %s)", model, strings.Join(anthropic.Models(), ", "))
			}
			pairs = append(pairs, configPair{tierModelKey(agents.TierMain), model})
		} else {
			if err := checkLocalModelID(model); err != nil {
				return nil, err
			}
			pairs = append(pairs, configPair{flags.LLMLocalModel, model})
		}
	}
	if given["base-url"] {
		if err := checkLocalBaseURL(baseURL); err != nil {
			return nil, err
		}
		pairs = append(pairs, configPair{flags.LLMLocalBaseURL, baseURL})
	}
	return pairs, nil
}

// checkLocalModelID refuses what cannot be one model id on one config line.
func checkLocalModelID(model string) error {
	if model == "" || len(model) > maxLocalModelLen {
		return fmt.Errorf("the local model id must be 1 to %d bytes", maxLocalModelLen)
	}
	for _, r := range model {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '#' || r == '"' || r == '\'' {
			return fmt.Errorf("the local model id %q contains a character a model id does not", model)
		}
	}
	return nil
}

// checkLocalBaseURL accepts what the local adapter can use: an http or https
// URL with a host, carrying no credential, query or fragment. A host that is
// not on this machine is allowed, as it is in the setting; the adapter then
// requires a key for it, and the status warns.
func checkLocalBaseURL(raw string) error {
	if raw != strings.TrimSpace(raw) || raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, " \t\r\n#\"'") {
		return fmt.Errorf("%q is not a base URL", raw)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("%q is not an http(s) base URL with a host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must not carry credentials, a query or a fragment", raw)
	}
	return nil
}

// useStatusFor reads the mode in force and what each mode would use, through
// the registry the next run builds.
func useStatusFor(reg *flags.Registry) useStatus {
	provider, origin, _ := reg.String(flags.LLMDefaultProvider)
	s := useStatus{Provider: provider, ProviderFrom: originName(origin), Mode: modeOf(provider),
		ShadowedBy: []string{}, Warnings: []string{}}
	if path, err := userConfigPath(); err == nil {
		s.UserConfig = path
	}
	envModel := strings.TrimSpace(os.Getenv("MANVI_MODEL"))

	cloudModel, _, _, _ := mainModel(reg)
	_, tierOrigin, _ := reg.String(tierModelKey(agents.TierMain))
	s.Cloud = useCloud{Model: cloudModel, ModelFrom: originName(tierOrigin)}
	if envModel != "" {
		s.Cloud.ModelFrom = originName(flags.OriginEnv)
	}

	base, baseOrigin, _ := reg.String(flags.LLMLocalBaseURL)
	localModel, localOrigin, _ := reg.String(flags.LLMLocalModel)
	s.Local = useLocal{BaseURL: base, BaseURLFrom: originName(baseOrigin), Model: strings.TrimSpace(localModel), ModelFrom: originName(localOrigin)}

	switch s.Mode {
	case modeCloud:
		s.Model, s.ModelFrom = s.Cloud.Model, s.Cloud.ModelFrom
	case modeLocal:
		s.Model, s.ModelFrom = s.Local.Model, s.Local.ModelFrom
		if envModel != "" {
			s.Model, s.ModelFrom = envModel, originName(flags.OriginEnv)
		}
		if !transport.IsLoopbackURL(base) {
			s.Warnings = append(s.Warnings, "the local base URL is not on this machine, so the local adapter will not send a request without a key for it (LOCAL_API_KEY)")
		}
	default:
		s.ModelFrom = originName(flags.OriginDefault)
		if envModel != "" {
			s.Model, s.ModelFrom = envModel, originName(flags.OriginEnv)
		}
	}

	shadow := func(key string, origin flags.Origin) {
		if origin == flags.OriginEnv {
			s.ShadowedBy = append(s.ShadowedBy, flags.EnvKey(key))
		}
	}
	shadow(flags.LLMDefaultProvider, origin)
	if envModel != "" {
		s.ShadowedBy = append(s.ShadowedBy, "MANVI_MODEL")
	}
	switch s.Mode {
	case modeCloud:
		shadow(tierModelKey(agents.TierMain), tierOrigin)
	case modeLocal:
		shadow(flags.LLMLocalBaseURL, baseOrigin)
		shadow(flags.LLMLocalModel, localOrigin)
	}
	return s
}

// originName is the origin as the host reads it: default, config, env or flag
// (the registry calls a value set on human authority an override).
func originName(o flags.Origin) string {
	if o == flags.OriginOverride {
		return "flag"
	}
	return string(o)
}

func printUseStatus(out io.Writer, s useStatus) error {
	var b strings.Builder
	if s.Mode == modeOther {
		fmt.Fprintf(&b, "manvi runs on %s (from %s)\n", s.Provider, s.ProviderFrom)
	} else {
		fmt.Fprintf(&b, "manvi runs on %s: %s (from %s)\n", s.Mode, s.Provider, s.ProviderFrom)
	}
	for _, name := range s.ShadowedBy {
		fmt.Fprintf(&b, "  %s is set in this environment and wins over the saved value for this run\n", name)
	}
	if s.Model != "" {
		fmt.Fprintf(&b, "model  %s (from %s)\n", s.Model, s.ModelFrom)
	}
	fmt.Fprintf(&b, "cloud  main tier %s (from %s)\n", s.Cloud.Model, s.Cloud.ModelFrom)
	local := s.Local.Model
	if local == "" {
		local = "whatever the server serves"
	}
	fmt.Fprintf(&b, "local  %s (from %s) · model %s (from %s)\n", s.Local.BaseURL, s.Local.BaseURLFrom, local, s.Local.ModelFrom)
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	if s.UserConfig != "" {
		fmt.Fprintf(&b, "saved in %s; 'manvi use cloud' or 'manvi use local' switches every app that runs manvi\n", s.UserConfig)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// setUserConfigValues sets top-level keys in the user config, creating the file
// when there is none and keeping every other line as it was.
//
// It edits text rather than re-rendering parsed settings, so the operator's
// comments and order survive, and it checks the result by parsing it: every
// other setting must read back unchanged and each key written must read back as
// its value, or nothing is written. A key the file sets in nested form is not
// rewritten — there is no line to replace without restructuring the block — and
// the refusal says where to edit it.
func setUserConfigValues(path string, pairs []configPair) error {
	// A symlinked config is edited where it points, not replaced by the rename.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	before, err := flags.ParseConfigLayer(bytes.NewReader(old))
	if err != nil {
		return fmt.Errorf("%s does not parse, so it is not rewritten: %w", path, err)
	}
	var lines []string
	if len(old) > 0 {
		lines = strings.Split(strings.TrimRight(string(old), "\n"), "\n")
	}
	changed := map[string]string{}
	for _, p := range pairs {
		changed[p.key] = p.value
		entry := p.key + ": " + p.value
		if at, ok := before.Lines[p.key]; ok {
			i := at - 1
			if i < 0 || i >= len(lines) || !strings.HasPrefix(lines[i], p.key+":") {
				return fmt.Errorf("%s sets %s in nested form on line %d; change it there by hand", path, p.key, at)
			}
			lines[i] = entry
		} else {
			lines = append(lines, entry)
		}
	}
	updated := []byte(strings.Join(lines, "\n") + "\n")

	after, err := flags.ParseConfigLayer(bytes.NewReader(updated))
	if err != nil {
		return fmt.Errorf("the edited %s would not parse, so it was not written: %w", path, err)
	}
	expected := len(before.Values)
	for key, value := range changed {
		if after.Values[key] != value {
			return fmt.Errorf("the edited %s reads %s back as %q, not %q; it was not written", path, key, after.Values[key], value)
		}
		if _, had := before.Values[key]; !had {
			expected++
		}
	}
	for k, v := range before.Values {
		if _, ours := changed[k]; !ours && after.Values[k] != v {
			return fmt.Errorf("editing %s would change %s; it was not written", path, k)
		}
	}
	if len(after.Values) != expected {
		return fmt.Errorf("editing %s would add or drop a setting it was not asked to; it was not written", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, updated, 0o600)
}

// renameFile is os.Rename, replaceable so a test can fail the last step.
var renameFile = os.Rename

// writeFileAtomic replaces path through a temporary file in its directory, so a
// concurrent reader sees the old file or the new one, never half of either, and
// a failure at any step leaves the old file and no temporary behind.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return renameFile(name, path)
}
