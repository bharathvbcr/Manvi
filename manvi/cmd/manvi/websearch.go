package main

import (
	"fmt"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
)

// The hosted web search tool is the one place query text the model chose leaves
// this machine for a third party, so both of its settings are operator scope:
// never read from the repository's config.yaml, which ships with a clone and
// can be written by an agent. Defined here, beside the tier keys, because this
// harness alone reads them.
const (
	webSearchKey        = "llm.anthropic.web_search"
	webSearchMaxUsesKey = "llm.anthropic.web_search_max_uses"

	webSearchOn  = "on"
	webSearchOff = "off"

	// defaultWebSearchMaxUses is the per-request bound when search is on and
	// the operator named none. Single-fact lookups use one to three.
	defaultWebSearchMaxUses = 3
)

func webSearchFlagDefs() []flags.Def {
	return []flags.Def{{
		Key: webSearchKey, Kind: flags.KindEnum, Default: webSearchOff,
		Values: []string{webSearchOff, webSearchOn}, Mutable: flags.HumanOnly,
		Description: "Lets Anthropic-served agents use Anthropic's hosted web search tool (web_search_20250305). " +
			"Off by default. On means the query text the model writes is sent to Anthropic and its search " +
			"backend, outside this machine, and results come back as untrusted text this harness cannot frame. " +
			"Billed at $10 per 1,000 searches on top of tokens. Only requests that offer tools carry it. " +
			"Operator scope: never read from the repository's config.yaml.",
	}, {
		Key: webSearchMaxUsesKey, Kind: flags.KindInt, Default: fmt.Sprint(defaultWebSearchMaxUses), Mutable: flags.HumanOnly,
		Description: fmt.Sprintf("Most searches one request may make when %s is on (1 to %d). The provider "+
			"enforces it, and a request reserves that many searches against the budget ledger. Operator scope.",
			webSearchKey, anthropic.MaxWebSearchUses),
	}}
}

// isWebSearchKey reports whether key is one of the two web search settings.
func isWebSearchKey(key string) bool { return key == webSearchKey || key == webSearchMaxUsesKey }

// applyWebSearch turns the adapter's hosted search tool on as the registry says.
// A registry that cannot answer leaves it off, the safe direction for a setting
// that widens network reach; a value that is on and out of range is refused
// rather than clamped, because the bound is what the operator is paying for.
func applyWebSearch(adapter *anthropic.Adapter, reg *flags.Registry) error {
	mode, _, err := reg.String(webSearchKey)
	if err != nil || mode != webSearchOn {
		return nil
	}
	uses, _, err := reg.Int(webSearchMaxUsesKey)
	if err != nil {
		return fmt.Errorf("%s: %w", webSearchMaxUsesKey, err)
	}
	if uses < 1 || uses > anthropic.MaxWebSearchUses {
		return fmt.Errorf("%s=%d: web search is on and its bound must be 1 to %d", webSearchMaxUsesKey, uses, anthropic.MaxWebSearchUses)
	}
	return adapter.SetWebSearch(uses)
}
