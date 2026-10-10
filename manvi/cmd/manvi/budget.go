package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
)

// The Anthropic spend cap is opt-in and account-wide.
//
// It is read from the environment, as MANVI_MODEL is, because it describes the
// operator's account rather than a repository: the default ledger lives in the
// user's configuration directory, so every project and every concurrent
// session on this machine draws down the same limit.
const (
	envAnthropicBudget = "MANVI_ANTHROPIC_BUDGET_USD"
	envAnthropicLedger = "MANVI_ANTHROPIC_BUDGET_LEDGER"
)

// usdPattern is a plain dollar amount. Exponents, signs and the float
// spellings ParseFloat would accept ("1e2", "Inf") are refused: a cap is the
// one setting where a misread value spends real money.
var usdPattern = regexp.MustCompile(`^([0-9]{1,7})(?:\.([0-9]{1,9}))?$`)

// parseUSD converts a dollar amount to nano-USD exactly, without floats.
func parseUSD(raw string) (int64, error) {
	m := usdPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, fmt.Errorf("%q is not a plain dollar amount such as 200 or 49.50", raw)
	}
	whole, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	frac := int64(0)
	if m[2] != "" {
		if frac, err = strconv.ParseInt(m[2]+strings.Repeat("0", 9-len(m[2])), 10, 64); err != nil {
			return 0, err
		}
	}
	nano := whole*1_000_000_000 + frac
	if nano <= 0 || nano > 1_000_000*1_000_000_000 {
		return 0, fmt.Errorf("%q is outside ($0, $1,000,000]", raw)
	}
	return nano, nil
}

// anthropicLedgerPath is where the account-wide ledger lives.
func anthropicLedgerPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(envAnthropicLedger)); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s=%q must be an absolute path", envAnthropicLedger, p)
		}
		return filepath.Clean(p), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the budget ledger: %w (set %s)", err, envAnthropicLedger)
	}
	return filepath.Join(dir, "manvi", "budget", "anthropic.json"), nil
}

// meterAnthropic wraps the adapter in the spend cap when one is configured.
// A cap that is set but unusable fails construction: silently running
// unmetered is the one outcome an operator who set a cap did not ask for.
func meterAnthropic(p llm.Provider) (llm.Provider, error) {
	raw := strings.TrimSpace(os.Getenv(envAnthropicBudget))
	if raw == "" {
		return p, nil
	}
	limit, err := parseUSD(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envAnthropicBudget, err)
	}
	path, err := anthropicLedgerPath()
	if err != nil {
		return nil, err
	}
	ledger, err := budget.Open(path, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envAnthropicBudget, err)
	}
	return &budget.Provider{Inner: p, Ledger: ledger}, nil
}

// meteredAnthropicEnv names the variables a managed Claude agent must not
// inherit while a cap is set. That agent is a separate process speaking to the
// API itself: handed the metered key, it would spend from the same account
// with nothing recording it. Withheld, it authenticates however it otherwise
// would — its own login — or fails to start and says so.
func meteredAnthropicEnv() []string {
	if strings.TrimSpace(os.Getenv(envAnthropicBudget)) == "" {
		return nil
	}
	for _, req := range credentials.DefaultRequirements() {
		if req.Provider == anthropic.Name {
			return append([]string(nil), req.EnvVars...)
		}
	}
	return nil
}

// budgetCommand is `manvi budget [reconcile]`.
func budgetCommand(out io.Writer, args []string) error {
	switch {
	case len(args) == 0:
		return showBudget(out)
	case len(args) == 1 && args[0] == "reconcile":
		return reconcileBudget(out)
	default:
		return fmt.Errorf("usage: budget [reconcile]")
	}
}

// reconcileBudget retains the reservations of attempts whose process exited
// before resolving them. They stay counted against the limit — they may have
// been billed — but they stop occupying in-flight slots.
func reconcileBudget(out io.Writer) error {
	path, err := anthropicLedgerPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "no ledger at %s; nothing to reconcile\n", path)
		return nil
	}
	reclaimed, err := budget.ReconcileAt(path)
	if err != nil {
		return err
	}
	if len(reclaimed) == 0 {
		fmt.Fprintln(out, "every in-flight attempt belongs to a running process; nothing reclaimed")
		return nil
	}
	var total int64
	for _, a := range reclaimed {
		total += a.ReservedNanoUSD
		fmt.Fprintf(out, "retained #%d %s %s (pid %d exited)\n", a.ID, a.Model, budget.USD(a.ReservedNanoUSD), a.PID)
	}
	fmt.Fprintf(out, "%d attempts, %s, moved from in flight to retained; still counted against the limit\n", len(reclaimed), budget.USD(total))
	return nil
}

// showBudget reports the Anthropic ledger.
func showBudget(out io.Writer) error {
	path, err := anthropicLedgerPath()
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "anthropic budget")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  ledger     %s\n", path)
	raw := strings.TrimSpace(os.Getenv(envAnthropicBudget))
	var limit int64
	if raw == "" {
		fmt.Fprintf(out, "  limit      none — set %s to cap spend (e.g. %s=200)\n", envAnthropicBudget, envAnthropicBudget)
	} else if limit, err = parseUSD(raw); err != nil {
		return fmt.Errorf("%s: %w", envAnthropicBudget, err)
	} else {
		fmt.Fprintf(out, "  limit      %s\n", budget.USD(limit))
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(out, "  spent      $0.00 — no metered request yet")
		return nil
	}
	s, err := budget.Inspect(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  spent      %s over %d requests, priced from reported usage\n", budget.USD(s.SpentNanoUSD), s.Settled)
	fmt.Fprintf(out, "  retained   %s over %d attempts whose charge could not be confirmed (counted as spent)\n",
		budget.USD(s.RetainedNanoUSD), s.RetainedAttempts)
	if s.RefusedAttempts > 0 {
		fmt.Fprintf(out, "  refused    %d attempts rejected before processing (rate limited, overloaded, invalid); not counted\n", s.RefusedAttempts)
	}
	fmt.Fprintf(out, "  in flight  %s over %d attempts\n", budget.USD(s.InFlightNanoUSD()), len(s.InFlight))
	for _, a := range s.InFlight {
		fmt.Fprintf(out, "               #%d %s %s pid %d since %s\n", a.ID, a.Model, budget.USD(a.ReservedNanoUSD), a.PID,
			time.Unix(a.StartedUnix, 0).Format(time.RFC3339))
	}
	if limit > 0 {
		fmt.Fprintf(out, "  remaining  %s\n", budget.USD(max(limit-s.CommittedNanoUSD(), 0)))
	}
	if s.Overruns > 0 {
		fmt.Fprintf(out, "  warning    %d requests cost more than their reservation; the limit can be passed by that difference\n", s.Overruns)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  This ledger counts only requests Manvi metered; the Anthropic Console shows the account balance.")
	return nil
}
