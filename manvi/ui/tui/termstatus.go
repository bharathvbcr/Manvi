package tui

import (
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui/logo"
	"github.com/bharathvbcr/Manvi/manvi/ui/term"
)

// TermStatus is what the terminal's own chrome should say about this run: the
// title on its tab and the progress indicator beside it.
//
// It is for the operator who is not looking. The frame already says all of
// this to someone who is; the tab is what is visible from another window, and
// it has room for a few words, cut from the right. So the mark comes first,
// then the state, then the session, and the words are plain.
//
// The mark is the one-line ribbon and it does not move. It stands where another
// tool would put a spinner, and it is what tells this tab from the shell next
// to it. Whether a given terminal's tab font draws its two characters has not
// been checked anywhere; they are block elements like the ones the frame is
// built from, and where the terminal is not trusted with those the mark is the
// letter.
type TermStatus struct {
	Title    string
	Progress term.Progress
}

// titleMarkASCII is the mark on a terminal that is not drawn with block
// characters.
const titleMarkASCII = "M"

// titleMark is the mark a title leads with.
func (a *App) titleMark() string {
	if a.Unicode {
		return logo.RibbonText()
	}
	return titleMarkASCII
}

// What a running turn was last seen doing. "running <tool>" is built where the
// tool's name arrives.
const (
	phaseWorking  = "working"
	phaseThinking = "thinking"
	phaseWriting  = "writing"
)

// turnOutcome is how a session's last turn ended, kept until the operator has
// seen it.
type turnOutcome int

const (
	outcomeNone turnOutcome = iota
	outcomeDone
	outcomeFailed
)

// stallWarnSecs is how long a running turn may produce nothing before it is
// shown as stalled, on the tab and on the working row alike.
const stallWarnSecs = 30

// tabState is what one session has to say, in rising order of urgency: the
// order is what decides which session a single tab speaks for.
type tabState int

const (
	tabIdle tabState = iota
	tabDone
	tabRunning
	tabStalled
	tabFailed
	tabApproval
)

// tabState reports the session's state and the words for it.
func (v *AgentView) tabState(now time.Time) (tabState, string) {
	switch {
	case v.Approval() != nil:
		return tabApproval, "needs approval"
	case v.Status.Busy:
		if !v.lastActivity.IsZero() && now.Sub(v.lastActivity) >= stallWarnSecs*time.Second {
			// No count of seconds: it would be a new title every second.
			return tabStalled, "no progress"
		}
		if v.phase != "" {
			return tabRunning, v.phase
		}
		return tabRunning, phaseWorking
	case v.outcome == outcomeFailed:
		return tabFailed, "failed"
	case v.outcome == outcomeDone:
		return tabDone, "done"
	}
	return tabIdle, ""
}

// progressFor maps a state to the terminal's indicator. Running is the
// indeterminate one and never a percentage: a turn has no known length.
func progressFor(s tabState) term.Progress {
	switch s {
	case tabApproval, tabStalled:
		return term.ProgressPaused
	case tabFailed:
		return term.ProgressError
	case tabRunning:
		return term.ProgressBusy
	}
	return term.ProgressNone
}

// TermStatus reports what the tab should say now.
//
// One tab speaks for every session, so it speaks for the one with the most
// urgent thing to say, the session on screen winning a tie, and counts the
// others that are not idle. With no session at all it is the mark and the
// name.
//
// It changes when a state changes and not otherwise. Nothing in it spins: a
// title that animates is rewritten on every frame, and the terminal's own
// indicator is the part that is meant to move.
func (a *App) TermStatus() TermStatus {
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	var (
		lead  *AgentView
		state tabState
		words string
		busy  int
	)
	consider := func(v *AgentView) {
		s, w := v.tabState(now)
		if s != tabIdle {
			busy++
		}
		if lead == nil || s > state {
			lead, state, words = v, s, w
		}
	}
	// The session on screen first, so it keeps the tab against an equal.
	current := a.Current()
	if current != nil {
		consider(current)
	}
	for _, v := range a.views {
		if v != current {
			consider(v)
		}
	}
	if lead == nil {
		return TermStatus{Title: a.titleMark() + " " + logo.Name}
	}

	title := lead.Title
	if title == "" {
		title = lead.ID
	}
	if words != "" {
		title = words + " · " + title
		if busy > 1 {
			title += " (+" + itoa(busy-1) + ")"
		}
	}
	// The mark is the name, so the name is not spelled out again after it.
	return TermStatus{Title: a.titleMark() + " " + title, Progress: progressFor(state)}
}
