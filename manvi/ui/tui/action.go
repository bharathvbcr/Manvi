package tui

import (
	"context"

	"manvi/ui"
)

// Action is something that has happened and that the dispatcher must fold into
// state. Input becomes an Action; so does an event arriving from the harness,
// and so does the completion of an Effect.
//
// The vocabulary is closed and every member is a plain value. That is what makes
// the loop testable without a terminal: a test feeds Actions and asserts on
// state, with no goroutines, no timing, and no tty.
type Action interface{ action() }

// Input actions.
type (
	// ActionKey is a keystroke that no widget consumed.
	ActionKey struct{ Binding string }
	// ActionRune is printable text destined for whatever holds focus.
	ActionRune struct{ Runes []rune }
	// ActionPaste is pasted text.
	ActionPaste struct{ Text string }
	// ActionResize is a new terminal size.
	ActionResize struct{ W, H int }
	// ActionClick is a pointer press. Button follows input.MouseButton:
	// 1 is left, 2 middle, 3 right.
	ActionClick struct {
		X, Y   int
		Button int
	}
	// ActionRelease is a pointer button going up. Its main job is ending
	// whatever drag the matching press began.
	ActionRelease struct {
		X, Y   int
		Button int
	}
	// ActionMotion is the pointer moving. Button is the button held during
	// the move, or 0 for a bare hover — the terminal's any-motion mode is
	// what lets an overlay highlight follow the pointer before a click.
	ActionMotion struct {
		X, Y   int
		Button int
	}
	// ActionScroll is a wheel notch; Delta is rows, negative for up.
	ActionScroll struct {
		X, Y  int
		Delta int
	}
	// ActionTick advances animations.
	ActionTick struct{}
)

// Harness actions.
type (
	// ActionEvent is one event from the harness's event stream.
	ActionEvent struct {
		SessionID string
		Event     ui.Event
	}
	// ActionApprovalRequest is a blocked operation awaiting a decision.
	ActionApprovalRequest struct {
		SessionID string
		Request   ui.Request
		// Reply carries the answer back to the blocked caller. Buffered by the
		// sender, so a decision never blocks the UI loop.
		Reply chan ui.Decision
	}
	// ActionTurnStarted marks a session busy.
	ActionTurnStarted struct{ SessionID string }
	// ActionTurnEnded marks it idle. Err is non-nil if the turn failed.
	ActionTurnEnded struct {
		SessionID string
		Err       error
	}
	// ActionSessionAdded registers a new session.
	ActionSessionAdded struct {
		ID    string
		Title string
	}
	// ActionNotice is a message from an effect that produced no events.
	ActionNotice struct {
		SessionID string
		Text      string
		Status    StatusKind
	}
)

func (ActionKey) action()             {}
func (ActionRune) action()            {}
func (ActionPaste) action()           {}
func (ActionResize) action()          {}
func (ActionClick) action()           {}
func (ActionRelease) action()         {}
func (ActionMotion) action()          {}
func (ActionScroll) action()          {}
func (ActionTick) action()            {}
func (ActionEvent) action()           {}
func (ActionApprovalRequest) action() {}
func (ActionTurnStarted) action()     {}
func (ActionTurnEnded) action()       {}
func (ActionSessionAdded) action()    {}
func (ActionNotice) action()          {}

// Effect is work that must happen off the loop.
//
// Effects are values rather than closures so the dispatcher stays pure: it
// returns a description of what should happen, and the runner performs it. A
// dispatcher that called the harness directly could not be tested without one.
type Effect interface{ effect() }

// The effects.
type (
	// EffectSubmit sends a prompt to a session.
	EffectSubmit struct {
		SessionID string
		Text      string
	}
	// EffectCancel stops a session's running turn.
	EffectCancel struct{ SessionID string }
	// EffectCommand runs a harness command, which streams events back.
	EffectCommand struct {
		SessionID string
		Name      string
		Args      string
	}
	// EffectDecide answers an approval.
	EffectDecide struct {
		Reply    chan ui.Decision
		Decision ui.Decision
	}
	// EffectNewSession asks the host for another session.
	EffectNewSession struct{}
	// EffectCloseSession ends one.
	EffectCloseSession struct{ SessionID string }
	// EffectCopy puts text on the system clipboard.
	EffectCopy struct{ Text string }
	// EffectSuspend performs a Ctrl+Z.
	EffectSuspend struct{}
	// EffectRedraw forces a full repaint, after something else wrote to the tty.
	EffectRedraw struct{}
	// EffectQuit ends the session.
	EffectQuit struct{}
)

func (EffectSubmit) effect()       {}
func (EffectCancel) effect()       {}
func (EffectCommand) effect()      {}
func (EffectDecide) effect()       {}
func (EffectNewSession) effect()   {}
func (EffectCloseSession) effect() {}
func (EffectCopy) effect()         {}
func (EffectSuspend) effect()      {}
func (EffectRedraw) effect()       {}
func (EffectQuit) effect()         {}

// Host is everything the TUI can ask the harness to do.
//
// The interface exists so this package depends on no harness wiring. It is also
// the honesty seam: a host with no model credential returns an error from
// Submit, and the TUI renders that error, rather than either package pretending
// a turn ran.
type Host interface {
	// Submit runs a turn. Events are delivered to the sink the host was built
	// with; the returned error ends the turn.
	Submit(ctx context.Context, sessionID, text string) error
	// Cancel stops a running turn. It must be safe to call when nothing runs.
	Cancel(sessionID string)
	// Command runs a named harness command — the same code paths the CLI
	// subcommands use, not a reimplementation.
	Command(ctx context.Context, sessionID, name, args string) error
	// Commands lists what Command accepts, for the palette and completion.
	Commands() []CommandSpec
	// Settings lists every harness setting with its current value, for the
	// settings picker. It is a read: moving one goes back through Command, so
	// there is exactly one path that changes a setting and exactly one place
	// that reports, validates, and audits the change.
	Settings() []SettingSpec
	// NewSession creates a session and returns its id and title.
	NewSession(ctx context.Context) (string, string, error)
	// CloseSession releases whatever the session held — a lease, most
	// importantly, which outlives the process if it is not given back.
	CloseSession(ctx context.Context, sessionID string) error
}

// CommandSpec describes a slash command.
type CommandSpec struct {
	Name string
	// Args is a usage hint shown in the dropdown, e.g. "PATH --reason TEXT".
	Args string
	// Summary is one line.
	Summary string
	// Mutating marks a command that changes state, so the palette can show it
	// differently from a read-only inspection.
	Mutating bool
}

// SettingSpec is one harness setting as the settings picker shows it.
//
// It is a flattened view rather than the registry's own type because this
// package depends on no harness wiring, and because the picker needs the two
// facts the registry computes rather than stores: whether a safety setting is
// currently at its safest value, and whether it can be moved at all from here.
type SettingSpec struct {
	Key   string
	Value string
	// Origin is which layer supplied the value: default, config, env, override.
	Origin string
	// Mutable is who may move it: startup, human, agent.
	Mutable string
	// Safety marks a setting whose relaxed state weakens a gate.
	Safety bool
	// AtSafest is false only for a Safety setting sitting off its strictest
	// value. It is a separate field because Value alone cannot answer it — the
	// safest value is not always the default.
	AtSafest bool
	// Choices enumerates the legal values, or is empty when the value is
	// free-form. The picker shows them, because "expected one of [...]" is
	// otherwise something an operator learns by being refused.
	Choices []string
	// Summary is the setting's one-line description.
	Summary string
}
