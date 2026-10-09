package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// --- flags ---

// flagsCommand is the whole `flags` surface: the report, and the one way to
// move a setting.
//
// It returns the key it changed, or "" for a read. The caller is what knows
// whether anything has already read the old value — a CLI process is about to
// exit and has nothing to reload, an attended session has a gate and a provider
// that do — so the reload is the caller's to run and this reports rather than
// performs it.
func flagsCommand(out io.Writer, reg *flags.Registry, args []string, from surface) (string, error) {
	if len(args) > 0 && args[0] == "set" {
		return setFlag(out, reg, args[1:], from)
	}
	return "", showFlags(out, reg, args)
}

// surface is who is running the command, because "what happens next" has
// different true answers for each.
//
// A shell invocation is a process that is about to exit: nothing in it has
// cached the old value, so nothing reloads, and telling the operator that a
// reload happened would be the same class of untruth this command exists to
// remove from the flag table. An attended session has a gate and a provider
// built from the old value, and does reload them.
type surface int

const (
	surfaceShell surface = iota
	surfaceSession
)

// setFlag moves one setting on human authority.
//
// Human authority is the literal truth of both callers: a command line is a
// person at a keyboard, and the TUI reaches this only from a slash command the
// operator typed. Nothing the model emits can arrive here — the agent's tool
// surface has no path to it — which is what keeps the "an agent that can switch
// off its own write gate has no write gate" rule intact now that the seam is
// reachable at all.
func setFlag(out io.Writer, reg *flags.Registry, args []string, from surface) (string, error) {
	if len(args) < 2 {
		return "", errors.New("usage: manvi flags set KEY VALUE   (manvi flags --all lists every key)")
	}
	if len(args) > 2 {
		// Refused rather than joined or ignored. A value with a space in it is
		// not something this catalogue has, and silently dropping the tail
		// would set a flag to a prefix of what was typed.
		return "", fmt.Errorf("flags set takes exactly one value; got %d — quote it if it contains spaces", len(args)-1)
	}
	key, raw := args[0], args[1]

	def, ok := reg.Def(key)
	if !ok {
		return "", fmt.Errorf("unknown setting %q — run 'manvi flags --all' for the catalogue%s", key, nearestFlag(reg, key))
	}
	before, err := reg.Lookup(key)
	if err != nil {
		return "", err
	}
	if err := reg.Set(flags.Human, key, raw); err != nil {
		return "", err
	}
	after, err := reg.Lookup(key)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(out, "%s  %s (%s) → %s (%s)\n", key, before.Raw, before.Origin, after.Raw, after.Origin)
	// Said out loud, because "it was already that" and "it changed" look
	// identical in the line above and lead to different next actions — an
	// operator who believes they moved something is an operator who stops
	// looking for why the behaviour did not change.
	switch {
	case before == after:
		fmt.Fprintln(out, "\nnothing changed: it was already this value, from this layer")
	case before.Raw == after.Raw:
		fmt.Fprintln(out, "\nthe value did not change; only the layer it now comes from did")
	}
	if def.Safety {
		// Which direction it moved decides the sentence. Printing the weakened
		// warning for a flag just returned to its strictest value would teach
		// an operator to read the warning as noise, which is the reading that
		// makes it useless when it is the true one.
		if after.Raw == safestValue(def) {
			fmt.Fprintf(out, "\n! %s is a safety flag, and this is its safest value.\n", key)
		} else {
			fmt.Fprintf(out, "\n! %s is a safety flag and is no longer at its safest value (%s). "+
				"Every run that used it reports the value, so a result reached under this setting "+
				"cannot be read as a strict one.\n", key, safestValue(def))
		}
	}
	if from == surfaceSession {
		// Only where it is true. In a shell there is no session holding a copy
		// of the old value, so both of these would describe work nobody did.
		switch flags.ReachOf(def) {
		case flags.ReachNewSession:
			fmt.Fprintln(out, "\nsessions already open keep what they built at start; this applies to sessions opened from now on")
		case flags.ReachReload:
			fmt.Fprintln(out, "\nwhatever had already read the old value is reloaded before the next turn")
		}
	}
	// Last, and unconditional. The override layer lives in this process and
	// dies with it, so from a shell this command changes nothing that outlives
	// the exit — a trap worth spending three lines on, because the operator who
	// falls into it believes the setting is applied.
	fmt.Fprintf(out, "\nthis override lasts as long as this process. To make it durable, "+
		"export %s=%s, or write %s: %s into %s\n",
		flags.EnvKey(key), after.Raw, key, after.Raw, configPath())
	return key, nil
}

// safestValue is the value of a safety flag that represents the strictest
// posture. Def.Safest is empty for every flag whose Default is already that
// value, which is all of them but harness.posture.
func safestValue(def flags.Def) string {
	if def.Safest != "" {
		return def.Safest
	}
	return def.Default
}

// nearestFlag suggests a key when one is mistyped, or returns "".
//
// A mistyped key is refused rather than guessed at — this only decorates the
// refusal. The prefix match is deliberately dumb: a namespace typed correctly
// with the leaf wrong is the mistake that actually happens with dotted keys.
func nearestFlag(reg *flags.Registry, key string) string {
	stem := key
	if i := strings.LastIndexByte(key, '.'); i > 0 {
		stem = key[:i+1]
	}
	var near []string
	for _, k := range reg.Keys() {
		if strings.HasPrefix(k, stem) {
			near = append(near, k)
		}
	}
	if len(near) == 0 || len(near) > 8 {
		return ""
	}
	return "\ndid you mean: " + strings.Join(near, ", ")
}

// effectiveFlagValue resolves what a flag will actually be obeyed as, for the
// three settings the posture can overrule.
//
// The flag table used to print Value.Raw for every key, which made it byte
// identical under strict and under yolo for exactly the settings an operator
// reads it to learn: `! policy.file.mode enforce default` on a run where the
// gate enforces and on a run where it is off. EffectiveGateMode exists to stop
// that — its own doc says a report of "enforce" while the gate does not run is
// worse than no report — and doctor honours it while this table did not. Two
// commands reading the same registry must not disagree about what the gate will
// do, so both now ask the same resolver.
func effectiveFlagValue(reg *flags.Registry, key string) (flags.Value, error) {
	switch key {
	case flags.PolicyFileMode, flags.PolicyCommandMode:
		mode, origin, err := flags.EffectiveGateMode(reg, key)
		if err != nil {
			return flags.Value{}, err
		}
		return flags.Value{Key: key, Raw: mode, Origin: origin}, nil
	case flags.PolicyHardRules:
		on, origin, err := flags.EffectiveHardRules(reg)
		if err != nil {
			return flags.Value{}, err
		}
		return flags.Value{Key: key, Raw: strconv.FormatBool(on), Origin: origin}, nil
	default:
		return reg.Lookup(key)
	}
}

func showFlags(out io.Writer, reg *flags.Registry, args []string) error {
	all := slices.Contains(args, "--all")
	for _, key := range reg.Keys() {
		def, _ := reg.Def(key)
		value, err := effectiveFlagValue(reg, key)
		if err != nil {
			return err
		}
		if !all && value.Origin == flags.OriginDefault && !def.Safety {
			continue
		}
		marker := " "
		if def.Safety {
			marker = "!"
		}
		// The value column is what is in force. When the posture decided it,
		// the setting's own value is named after it rather than dropped: an
		// operator who typed `policy.file.mode: enforce` and is reading why the
		// gate is off needs to see both halves of that answer on one line.
		note := ""
		if set, err := reg.Lookup(key); err == nil && set.Raw != value.Raw {
			note = fmt.Sprintf("  (set to %s/%s)", set.Raw, set.Origin)
		}
		fmt.Fprintf(out, "%s %-34s %-14s %-9s %s%s\n", marker, key, value.Raw, value.Origin, def.Mutable, note)
	}
	if !all {
		fmt.Fprintln(out, "\n(showing safety flags and anything moved off its default; --all for everything)")
	}
	fmt.Fprintln(out, "\n! = safety flag: moving it is reported on every run that used it")
	fmt.Fprintln(out, "the last column is who may move it: 'human' means 'flags set KEY VALUE' from here, "+
		"'startup' means it is fixed for the life of this process")
	return nil
}
