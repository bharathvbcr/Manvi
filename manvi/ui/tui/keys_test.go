package tui

import "testing"

// TestAKeyBoundInAContextShadowsTheGlobalOne is the resolution rule, and the
// consequences are not academic. Ctrl+U deletes to the start of the line while
// the composer has focus and scrolls half a page in the transcript; Ctrl+K
// deletes to the end of the line and scrolls up. Getting the fallback wrong in
// either direction destroys a draft the operator was in the middle of typing.
func TestAKeyBoundInAContextShadowsTheGlobalOne(t *testing.T) {
	cases := []struct {
		key  string
		ctx  Context
		want Cmd
	}{
		{"ctrl+u", CtxPrompt, CmdDeleteToStart},
		{"ctrl+u", CtxScrollback, CmdHalfPageUp},
		{"ctrl+k", CtxPrompt, CmdDeleteToEnd},
		{"ctrl+k", CtxScrollback, CmdScrollUp},
		{"enter", CtxPrompt, CmdSubmit},
		{"enter", CtxScrollback, CmdToggleFold},
		{"enter", CtxApproval, CmdAccept},
		{"enter", CtxDashboard, CmdAccept},
		{"esc", CtxPrompt, CmdClearDraft},
		{"esc", CtxScrollback, CmdDismiss},
		{"esc", CtxApproval, CmdDismiss},
		{"up", CtxPrompt, CmdHistoryPrev},
		{"up", CtxScrollback, CmdSelectPrev},
		{"up", CtxDashboard, CmdPrevItem},
		// Unbound in the context, so the global table answers.
		{"ctrl+c", CtxPrompt, CmdCancel},
		{"ctrl+c", CtxScrollback, CmdCancel},
		{"tab", CtxPrompt, CmdFocusToggle},
		{"ctrl+p", CtxScrollback, CmdPalette},
	}
	for _, tc := range cases {
		if got := resolve(tc.key, tc.ctx); got != tc.want {
			t.Errorf("resolve(%q, %v) = %v, want %v", tc.key, tc.ctx, got, tc.want)
		}
	}
}

// TestCtrlCIsNeverQuit. In raw mode it is the only way to interrupt a turn, and
// a harness that exits on it loses the transcript of whatever the operator was
// trying to stop. It is asserted across every context because a single context
// binding it to something else would be enough to lose that.
func TestCtrlCIsNeverQuit(t *testing.T) {
	for _, ctx := range []Context{
		CtxGlobal, CtxPrompt, CtxScrollback, CtxApproval, CtxDashboard, CtxOverlay,
	} {
		if got := resolve("ctrl+c", ctx); got == CmdQuit {
			t.Errorf("ctrl+c quits in %v", ctx)
		}
	}
}

// TestQuitIsReachableFromWhereverTheOperatorIs. Ctrl+D is bound to quit
// globally but shadowed by an editing command in both contexts an operator
// spends their time in, so it quits from the dashboard and deletes a character
// in the composer — the same keystroke, two very different outcomes. Ctrl+Q is
// what makes quit reachable everywhere, and this pins that.
func TestQuitIsReachableFromWhereverTheOperatorIs(t *testing.T) {
	for _, ctx := range []Context{CtxGlobal, CtxPrompt, CtxScrollback, CtxDashboard} {
		if got := resolve("ctrl+q", ctx); got != CmdQuit {
			t.Errorf("ctrl+q in %v resolves to %v, not quit", ctx, got)
		}
	}
	// The asymmetry, recorded so a change to it is deliberate rather than
	// accidental.
	if resolve("ctrl+d", CtxPrompt) == CmdQuit {
		t.Error("ctrl+d quits from the composer; a delete key that exits the app is a data-loss key")
	}
	if resolve("ctrl+d", CtxDashboard) != CmdQuit {
		t.Error("ctrl+d no longer quits from the dashboard; this test records the asymmetry and needs updating")
	}
}

// TestEveryBindingResolvesToItself catches a table entry whose keys are listed
// under a context it is not reachable from — a binding that exists in the
// source and can never fire.
func TestEveryBindingResolvesToItself(t *testing.T) {
	for _, b := range bindings {
		for _, k := range b.Keys {
			if got := resolve(k, b.Ctx); got != b.Cmd {
				t.Errorf("%q in %v resolves to %v, not the %v it is declared for — "+
					"an earlier entry shadows it and this binding can never fire",
					k, b.Ctx, got, b.Cmd)
			}
		}
	}
}

// TestAnUnboundKeyIsNoCommand rather than falling through to something.
func TestAnUnboundKeyIsNoCommand(t *testing.T) {
	for _, ctx := range []Context{CtxGlobal, CtxPrompt, CtxScrollback, CtxDashboard, CtxApproval} {
		if got := resolve("ctrl+alt+meta+zzz", ctx); got != CmdNone {
			t.Errorf("an unbound key in %v resolved to %v", ctx, got)
		}
	}
}
