//go:build ruleguard

// Package gorules holds this repository's own invariants, written as
// go-ruleguard patterns so a tool enforces them rather than review.
//
// A rule earns its place here only when it encodes a claim the repository
// already makes in prose, and only when it does not fire on code that is
// already correct. Two candidates were cut for failing the second test, and
// the reasons are recorded because they are the interesting part:
//
//   - A `time.After` rule was written and dropped. It encodes advice that
//     stopped being true in Go 1.23: `internal/godebugs/table.go` records
//     asynctimerchan as Changed:23, and `go doc time.After` now says the
//     collector recovers unreferenced, unstopped timers. This module declares
//     go 1.26, so the new behaviour is the one in force. The rule would have
//     had 47 hits, every one of them wrong.
//
//   - An `exec.Command` rule was written and dropped. Its one production hit,
//     mcp/client.go:227, spawns a server that deliberately outlives any single
//     request: it gets its own process group and a WaitDelay instead. Binding
//     that child to a turn's context would be the defect, not the fix.
//
// This file is excluded from every build configuration by the `ruleguard` tag.
// The DSL import is a lint-only requirement in go.mod — `go list -deps ./...`
// reports no external packages, and no shipped binary contains it.
package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// secretThroughFmt enforces what credentials/credentials.go states in its
// package comment: the revealed value is "never handed a value that has been
// through fmt". Secret redacts under every verb, but Reveal returns a plain
// string, and a plain string in a format call is a token in a log line.
func secretThroughFmt(m dsl.Matcher) {
	m.Match(
		`fmt.Sprintf($*_, $x, $*_)`,
		`fmt.Sprint($*_, $x, $*_)`,
		`fmt.Sprintln($*_, $x, $*_)`,
		`fmt.Errorf($*_, $x, $*_)`,
		`fmt.Printf($*_, $x, $*_)`,
		`fmt.Println($*_, $x, $*_)`,
		`fmt.Fprintf($*_, $x, $*_)`,
		`fmt.Fprintln($*_, $x, $*_)`,
	).
		Where(m["x"].Text.Matches(`\.Reveal\(\)$`)).
		Report(`credential reaches fmt: hand Reveal() straight to its consumer (credentials/credentials.go, package comment)`)
}

// revealErrorDropped is what replaced secretParked, and the swap is worth
// recording because it is the shape a stale lint rule takes.
//
// secretParked forbade binding the result of Reveal to a variable: with the old
// `Reveal() string`, every legitimate call handed the value straight to a header
// Set, so a binding was the step before a leak. Sealing credentials in a
// memguard enclave made Reveal fallible, and `key, err := secret.Reveal()` is
// now the only way to call it — the rule forbade the correct code and fired on
// nothing, which is the same thing as not existing while looking like a
// guarantee.
//
// The fallible signature brought its own hazard, and this is it. Dropping the
// error yields an empty key, and an empty key is a 401 that reads like a bad
// credential — sending whoever is holding the pager to the vendor's console
// instead of to the enclave that failed to open. errcheck does not cover this:
// its check-blank option is off by default, and turning it on tree-wide would
// report every deliberate `_ =` in the harness.
func revealErrorDropped(m dsl.Matcher) {
	m.Match(
		`$_, _ := $x.Reveal()`,
		`$_, _ = $x.Reveal()`,
		`$_, _ := $x.RevealBytes($*_)`,
		`$_, _ = $x.RevealBytes($*_)`,
	).
		Report(`credential opened without checking the error: a dropped error here yields an empty key, which fails as a bad credential rather than as an enclave that would not open`)
}

// verdictAssertedNotMeasured is README "The Approach" item 3 stated as code: "A
// check that did not run never reports as passed." A field whose name asserts
// that a check happened must be set from that check's own result.
//
// Only the true direction is reported. SubAgentVerdict.Reconcile sets Passed to
// false from a literal, and that is the invariant working — failing closed is
// always safe, so a rule that flagged it would be pushing in the wrong
// direction.
func verdictAssertedNotMeasured(m dsl.Matcher) {
	m.Match(`$_.$f = true`).
		Where(m["f"].Text.Matches(`^(Verified|Validated|Checked|Passed|Certified|Audited|Approved)$`)).
		Report(`verdict set from a literal: assign it from the check's own result, or it reports a state nothing established (README, "non-cheating invariants")`)
}

// unboundedPeerRead is the payload half of the bound that fetch.Client already
// applies. fetch.go:245 wraps every response body in io.LimitReader because a
// peer otherwise chooses how much memory this process spends. io.ReadAll over a
// body or a child's pipe is that same unbounded read without the bound.
func unboundedPeerRead(m dsl.Matcher) {
	m.Match(
		`io.ReadAll($x.Body)`,
		`io.ReadAll($x.Stdout)`,
		`io.ReadAll($x.Stderr)`,
	).
		Report(`unbounded read of a peer-controlled stream: wrap it in io.LimitReader, the way fetch.Client bounds bodies with MaxBytes`)
}

// unexplainedSkip is the narrow, defensible remainder of a rule that started
// much wider. A blanket ban on t.Skip was written and dropped: 44 of its 45
// hits gate on a capability the machine either has or does not — windows,
// symlinks, FIFOs, /dev/ptmx, running as root — and a test that cannot be set
// up is not a test that was silently waived.
//
// What survives is the skip that says nothing. verify.sh already refuses to
// certify a run when MANVI_TEST_ALLOW_SKIP is set, because "a package whose
// tests all skip still prints ok"; a skip with no message is that same hole
// with no way to tell from the output which case went unexamined.
func unexplainedSkip(m dsl.Matcher) {
	m.Match(`$t.Skip()`, `$t.SkipNow()`).
		Where(m["t"].Type.Is(`*testing.T`) || m["t"].Type.Is(`*testing.B`) || m["t"].Type.Is(`*testing.F`)).
		Report(`skip with no reason: the run reports "ok" and nothing says which case went unexamined — state the condition being gated on`)
}
