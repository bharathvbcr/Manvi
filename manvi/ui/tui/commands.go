package tui

import (
	"strings"
)

// uiCommands are the commands this package answers itself, with no harness
// involved.
//
// They are listed rather than only handled because a command that appears in
// neither menu is a command an operator has to already know exists. /sessions
// and /theme were exactly that: implemented, reachable by typing the name in
// full, and absent from the palette and the completion dropdown that are the
// only two places anyone goes looking.
var uiCommands = []CommandSpec{
	{Name: "settings", Summary: "browse every setting, its value, and where the value came from"},
	{Name: "sessions", Summary: "switch between open sessions"},
	{Name: "theme", Summary: "choose a colour palette"},
	{Name: "help", Summary: "show keyboard shortcuts and help"},
	{Name: "clear", Summary: "clear this transcript"},
	{Name: "quit", Summary: "leave manvi"},
}

// menuCommands is what the palette and the completion dropdown list: the
// host's commands, plus the ones this package answers that the host does not
// already declare.
func (a *App) menuCommands() []CommandSpec {
	var specs []CommandSpec
	seen := map[string]bool{}
	if a.Host != nil {
		for _, spec := range a.Host.Commands() {
			specs = append(specs, spec)
			seen[spec.Name] = true
		}
	}
	for _, spec := range uiCommands {
		if !seen[spec.Name] {
			specs = append(specs, spec)
		}
	}
	return specs
}

// commandSpec finds a command's declared shape, or reports that neither the
// host nor this package offers it.
func (a *App) commandSpec(name string) (CommandSpec, bool) {
	for _, spec := range a.menuCommands() {
		if spec.Name == name {
			return spec, true
		}
	}
	return CommandSpec{}, false
}

// openSettings shows every harness setting as a list that can be navigated.
func (a *App) openSettings() []Effect {
	if a.Host == nil {
		return nil
	}
	specs := a.Host.Settings()
	if len(specs) == 0 {
		a.setNotice("this harness reports no settings", StatusWarn)
		return nil
	}
	a.overlay = SettingsOverlay(specs)
	return nil
}

// settingSpec finds a setting by key from the host's current report.
func (a *App) settingSpec(key string) (SettingSpec, bool) {
	if a.Host == nil {
		return SettingSpec{}, false
	}
	for _, s := range a.Host.Settings() {
		if s.Key == key {
			return s, true
		}
	}
	return SettingSpec{}, false
}

// takesArgs reports whether choosing this command from a menu should stop and
// let the operator type its arguments.
//
// Both menus used to run the chosen command with no arguments at all. The
// palette displayed the usage — "[--all]", "list|acquire|release",
// "PATH --reason TEXT" — in the same row it then discarded, so every option
// every command has was unreachable from the menus that advertised them.
func (a *App) takesArgs(name string) bool {
	spec, ok := a.commandSpec(name)
	return ok && spec.Args != ""
}

// armCommand puts a command that takes arguments into the composer instead of
// running it bare.
//
// It refuses to overwrite a draft. The palette is reachable with half a prompt
// already typed, and silently replacing that text with "/lease " would destroy
// work to save a keystroke — so the draft wins and the operator is told why.
func (a *App) armCommand(name string) []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	spec, _ := a.commandSpec(name)
	if !v.Prompt.Empty() {
		a.setNotice("/"+name+" takes "+spec.Args+
			" — the composer has a draft, so it was not replaced; clear it with esc and choose again", StatusWarn)
		return nil
	}
	v.Focus = CtxPrompt
	v.Prompt.SetValue("/" + name + " ")
	a.setNotice("/"+name+" "+spec.Args+" — enter sends it", StatusInfo)
	return nil
}

// armSetting writes the command that would move a setting into the composer,
// with its current value and legal values on the shortcut bar.
//
// It arms rather than sets, and that is the design rather than a shortcut.
// Every move then goes through /flags set, which is the one place that
// validates the value, refuses a startup flag, says which direction a safety
// flag moved, reloads what the change invalidated, and writes the whole thing
// into the transcript. A picker that flipped policy.file.mode with one keypress
// would be a second way to change a setting, and the second way is the one with
// no record.
func (a *App) armSetting(key string) []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	spec, ok := a.settingSpec(key)
	if !ok {
		a.setNotice("no setting "+key, StatusWarn)
		return nil
	}
	if spec.Mutable == "startup" {
		// Named rather than armed. The registry is sealed once boot completes,
		// so the command this would compose is certain to be refused, and a
		// picker that spends the operator's keystroke on a guaranteed failure
		// is a picker they stop trusting.
		a.setNotice(key+" is startup-only and cannot be moved from here — it is "+
			spec.Value+" ("+spec.Origin+")", StatusWarn)
		return nil
	}
	if !v.Prompt.Empty() {
		a.setNotice("the composer has a draft, so it was not replaced; clear it with esc and choose "+
			key+" again", StatusWarn)
		return nil
	}
	v.Focus = CtxPrompt
	v.Prompt.SetValue("/flags set " + key + " ")

	hint := key + " is " + spec.Value + " (" + spec.Origin + ")"
	if len(spec.Choices) > 0 {
		hint += " — one of [" + strings.Join(spec.Choices, ", ") + "]"
	}
	status := StatusInfo
	if spec.Safety {
		hint += " — safety setting"
		status = StatusWarn
	}
	a.setNotice(hint, status)
	return nil
}

func (a *App) runCommand(name, args string) []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	if a.Host != nil {
		known := false
		for _, spec := range a.Host.Commands() {
			if spec.Name == name {
				known = true
				break
			}
		}
		if !known {
			for _, spec := range uiCommands {
				if spec.Name == name {
					known = true
					break
				}
			}
		}
		if !known {
			// The aliases. They are not listed in either menu — one spelling
			// per command is enough to find it by — but they still resolve.
			switch name {
			case "exit", "cls":
				known = true
			}
		}
		if !known {
			a.setNotice("no command /"+name+" — ctrl+p to list", StatusWarn)
			return nil
		}
	}
	switch name {
	case "quit", "exit":
		return a.requestQuit()
	case "help":
		a.overlay = HelpOverlay()
		return nil
	case "clear", "cls":
		if v != nil {
			v.Scroll = NewScrollback()
		}
		return nil
	case "sessions":
		// The picker and its selection handler were both written; nothing ever
		// opened it. acceptOverlay has switched a.active on OverlaySessions
		// since the overlay was added, so this line is the whole of the missing
		// feature.
		a.overlay = SessionsOverlay(a.views, a.active)
		return nil
	case "theme":
		a.overlay = ThemeOverlay(a.Theme.Name)
		return nil
	case "settings":
		return a.openSettings()
	}

	// Everything above this line is answered by the UI and stays available
	// during a turn. Everything below is run by the harness, and the harness
	// runs one turn per session — a second one is dropped. The composer already
	// queues a prompt typed during a turn and says so; a slash command took a
	// different path, was dropped by the runner without a word, and looked from
	// here like the harness had hung.
	if v.Status.Busy {
		a.setNotice("/"+name+" needs this session idle — a turn is running; ctrl+c cancels it", StatusWarn)
		return nil
	}
	return []Effect{EffectCommand{SessionID: v.ID, Name: name, Args: args}}
}

func (a *App) openPalette() {
	var items []Item
	for _, spec := range a.menuCommands() {
		status := StatusNeutral
		if spec.Mutating {
			status = StatusWarn
		}
		detail := spec.Summary
		if spec.Args != "" {
			detail = spec.Args + "   " + detail
		}
		items = append(items, Item{
			Label: "/" + spec.Name, Detail: detail, Value: spec.Name, Status: status,
		})
	}
	a.overlay = NewOverlay(OverlayPalette, "commands", items, true)
}

// refreshCompletion opens, updates, or closes the inline dropdown to match what
// the prompt currently holds.
func (a *App) refreshCompletion() {
	v := a.Current()
	if v == nil || a.Host == nil {
		return
	}
	if a.overlay != nil && a.overlay.Kind != OverlayComplete {
		return
	}
	if v.Approval() != nil || v.Focus != CtxPrompt {
		return
	}
	trigger := v.Prompt.ActiveTrigger()
	if trigger.Kind != '/' {
		if a.overlay != nil && a.overlay.Kind == OverlayComplete {
			a.overlay = nil
		}
		return
	}
	var items []Item
	for _, spec := range a.menuCommands() {
		status := StatusNeutral
		if spec.Mutating {
			status = StatusWarn
		}
		// The usage hint, which CommandSpec.Args documents itself as being
		// "shown in the dropdown" and was shown only in the palette. A command
		// whose arguments are invisible where it is chosen is a command whose
		// arguments do not exist as far as the operator is concerned.
		detail := spec.Summary
		if spec.Args != "" {
			detail = spec.Args + "   " + detail
		}
		items = append(items, Item{
			Label: spec.Name, Detail: detail, Value: spec.Name, Status: status,
		})
	}
	o := NewOverlay(OverlayComplete, "commands", items, false)
	o.Trigger = trigger
	o.SetFilter(trigger.Query)
	if o.Empty() {
		a.overlay = nil
		return
	}
	a.overlay = o
}
