package devcouncil

import (
	"sort"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// Read authorisation has one owner, and this is it.
//
// read_file judged a path before it read it, and every other tool that can
// return a file's bytes — grep, get_diff, git_show, the code graph's source
// spans — either judged nothing or judged the name alone. A name is not the
// file: a hard link gives one inode a second name no pattern matches, a
// working-tree diff renders files nobody asked for by name, a commit's patch
// carries paths long since deleted, and the process's working directory can
// resolve a short name to a different file than the one that was judged.
//
// readRefusal answers the per-file question for a live path; withholdDiff and
// withholdHistoryDiff answer it for text git already rendered. A refusal always
// names the path and the rule (see withheldRead), so a filtered result is never
// presented as a complete one. TestEveryContentReaderAppliesTheReadGate keeps a
// new tool that returns repository bytes from being added without choosing
// one of them.

// Rules a withheld read is reported under. The first is policy's own read
// rule; the others have no spelling to match and so live here.
const (
	// ruleAliasedRead: the file has more than one name, so the secret-path
	// patterns cannot say what its contents are. The same refusal ReadPinned
	// and git stage make, for the same reason.
	ruleAliasedRead = "path.aliased_read"
	// ruleOutsideRoot: the path resolves outside the repository.
	ruleOutsideRoot = "path.outside_root"
)

// withheldRemedy is what a person can do about a read the gate refused, as a
// clause to follow the rule's name, or "" for a rule with nothing to say beyond
// its name.
func withheldRemedy(rule string) string {
	switch rule {
	case ruleAliasedRead:
		return ": the file has more than one name (a hard link), so reading it would also reach it under a name " +
			"the gate never judged. Replace it with a copy of the file to have it read"
	case ruleOutsideRoot:
		return ": it resolves outside the repository (a symbolic link out of it, or a path that climbs out). " +
			"Replace it with a copy of the file inside the repository to have it read"
	case string(policy.RuleSecretRead):
		return ": its name marks it as a place credentials live. Rename it if it holds none"
	}
	return ""
}

// withheldRead is one path a reader left out of its result, and the rule that
// left it out.
type withheldRead struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// readRefusal reports why a repository-relative path may not be shown to the
// model, or "" when it may. It judges the name through policy's own read
// classifier and then the file's link count, and it fails closed: a path whose
// link count cannot be verified is refused.
func (r *Registry) readRefusal(rel string) string { return readRefusalAt(r.deps.Root, rel) }

// readRefusalAt is readRefusal for a repository root that has no Registry: the
// one judgement, so a caller that only needs a gated reader (a host op reading
// a spec) cannot judge a path more leniently than read_file does.
func readRefusalAt(root, rel string) string {
	switch policy.ClassifyRead(root, rel) {
	case policy.ReadOutsideRoot:
		return ruleOutsideRoot
	case policy.ReadSecretPath:
		return string(policy.RuleSecretRead)
	}
	if len(aliasedPaths(root, []string{rel})) > 0 {
		return ruleAliasedRead
	}
	return ""
}

// withholdDiff removes from a working-tree diff every file section the read
// gate refuses, by the names git wrote into it. It is applied to what a model
// is shown and not to what the verifier scans: the secret scanner has to see
// the credential to flag it.
func (r *Registry) withholdDiff(diff string) (string, []withheldRead) {
	kept, withheld := policy.WithholdDiff(diff, func(name string) string {
		if policy.SecretPathSpelling(name) {
			return string(policy.RuleSecretRead)
		}
		if len(aliasedPaths(r.deps.Root, []string{name})) > 0 {
			return ruleAliasedRead
		}
		return ""
	})
	return kept, convertWithheld(withheld)
}

// withholdHistoryDiff is withholdDiff for a patch out of history. A name in a
// past commit says nothing about which file a link count would describe now,
// so only the secret-path rule applies.
func withholdHistoryDiff(diff string) (string, []withheldRead) {
	kept, withheld := policy.WithholdDiff(diff, func(name string) string {
		if policy.SecretPathSpelling(name) {
			return string(policy.RuleSecretRead)
		}
		return ""
	})
	return kept, convertWithheld(withheld)
}

func convertWithheld(in []policy.WithheldDiff) []withheldRead {
	out := make([]withheldRead, 0, len(in))
	for _, w := range in {
		rule := w.Rule
		if rule == policy.WithheldSecretPath {
			rule = string(policy.RuleSecretRead)
		}
		out = append(out, withheldRead{Path: w.Path, Rule: rule})
	}
	return out
}

// withheldPayload renders what a reader left out, for the result the model
// reads. nil when nothing was withheld, so a clean result carries no note.
func withheldPayload(withheld []withheldRead) map[string]any {
	if len(withheld) == 0 {
		return nil
	}
	sorted := append([]withheldRead(nil), withheld...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	return map[string]any{
		"paths": sorted,
		"note": "these paths were withheld by the read gate, so this result does not cover them; " +
			"each entry names the rule that applied",
	}
}
