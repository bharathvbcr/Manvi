package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// showAgents prints the role catalogue a session dispatches from: each role,
// the tier it is capped at, and the model that tier resolves to for the
// session's provider.
//
// It reads the same catalogue and the same profile a run attaches, so what it
// prints is where a child of that role would actually be sent.
func showAgents(out io.Writer, reg *flags.Registry) error {
	provider, _, err := reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return fmt.Errorf("reading %s: %w", flags.LLMDefaultProvider, err)
	}
	// Resolved, not checked: this is a diagnostic, and a profile a session
	// would refuse is reported by 'manvi tiers', which names the line to fix.
	table, err := resolveTiers(reg, provider)
	if err != nil {
		return err
	}
	profile := table.Profile()

	// Composed whole and written once, so a failed write is one error to
	// report rather than six to drop.
	var b strings.Builder
	fmt.Fprintf(&b, "agents — role → tier → model on provider %s\n\n", provider)
	fmt.Fprintf(&b, "  %-16s %-8s %-28s %s\n", "ROLE", "TIER", "MODEL", "WRITES")
	// The main agent's model is the session's own. On anthropic the table's
	// main row is what resolveModelFor sends — MANVI_MODEL, else llm.tier.main;
	// elsewhere only MANVI_MODEL is known without asking the provider, so this
	// row never names a model the session is not running.
	mainModel := "the session's model"
	if table.Active {
		mainModel = table.row(agents.TierMain).Model
	} else if m := strings.TrimSpace(os.Getenv("MANVI_MODEL")); m != "" {
		mainModel = m
	}
	fmt.Fprintf(&b, "  %-16s %-8s %-28s %s\n", "(main agent)", agents.TierMain, mainModel, "yes")
	for _, def := range agents.NewRegistry().List() {
		model := tierModel(profile, def.Ceiling())
		if !agents.ParsePlacement(def.Model, nil).Inherits() {
			model = def.Model + " (operator placement)"
		}
		writes := "no"
		if def.EnableWriteTools {
			writes = "yes"
		}
		fmt.Fprintf(&b, "  %-16s %-8s %-28s %s\n", def.Name, def.Ceiling(), model, writes)
	}
	fmt.Fprintf(&b, "\nA role's tier is the dearest it runs on. A dispatch may route it lower: a read-only "+
		"child goes to lite, or to basic in a fan-out of %d or more, and only a verdict reaches advisor.\n",
		agents.BasicFanoutWidth)
	if profile == nil {
		fmt.Fprintf(&b, "Tiers resolve on anthropic only; on %s every child runs on the session's model.\n", provider)
	}
	_, err = io.WriteString(out, b.String())
	return err
}

// tierModel is the model a tier resolves to, or what a child on it inherits.
func tierModel(profile agents.TierProfile, tier agents.Tier) string {
	if spec, ok := profile[tier]; ok && spec.Model != "" {
		return spec.Model
	}
	return "the session's model"
}
