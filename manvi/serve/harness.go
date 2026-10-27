package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/harness"
	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

// OpHarnessResolve resolves a harness spec a host names or supplies and returns
// it as sections for the host to compose into its own system prompt.
//
// It is an optional op: it is registered only when a HarnessModule is
// configured, so it is listed in hello's ops only on a build that serves it. A
// host that does not see it there has no harness to resolve and must run
// without one; it must not guess the op exists, and a build that predates it
// answers an unknown-op refusal, which is the same thing said later.
//
// What it does not do: it does not change a session. The chat plane is advisory
// (see chat.go): the host owns the request, so the host owns composing System.
// The recommended composition is the host's own essentials first, then these
// sections in the order given, then the skills listing; a section can only sit
// at or after harness.MinSectionOrder, so a spec never lands between a host's
// rules. The digest is what the host passes back as chat.prepare's
// harness_digest, so the compaction ledger can tell a harness change from churn.
//
// Authority is the one dh-1 gives a spec: a host may only tighten. The spec is
// decoded by harness.DecodeSpec and judged by Spec.Validate, so a flag, tier,
// posture, MCP, verify, fetch or write-exception field is refused by name, an
// Essential or otherwise reserved section name is refused, and every problem is
// listed, not the first. A spec is data and is never executed.
const OpHarnessResolve = "harness.resolve"

// Harness error codes. Both are non-retryable: the same spec fails the same way.
const (
	// ErrHarnessRefused means the spec was found or supplied and is refused: it
	// carries a field a spec may not carry, or fails validation. The message
	// lists every problem.
	ErrHarnessRefused = "E_HARNESS_REFUSED"
	// ErrHarnessNotFound means no spec has that name in the place named.
	ErrHarnessNotFound = "E_HARNESS_NOT_FOUND"
)

// Harness spec kinds.
const (
	HarnessKindRepo   = "repo"
	HarnessKindNamed  = "named"
	HarnessKindInline = "inline"
)

// HarnessSpecRef says which spec: a repository's own, a registered named one, or
// one the host carries.
type HarnessSpecRef struct {
	// Kind is "repo" (.devcouncil/harness/<name>.json in Root), "named" (a spec
	// the process registered) or "inline". An unknown kind is refused.
	Kind string `json:"kind"`
	// Name selects a repo or named spec. For inline it is optional and, when
	// given, must equal the inline spec's own name.
	Name string `json:"name,omitempty"`
	// Inline is the spec document itself, for kind inline. It is decoded
	// strictly: exact lowercase keys, no duplicates, no unknown field.
	Inline json.RawMessage `json:"inline,omitempty"`
}

// HarnessResolveParams asks for one spec resolved for one session.
type HarnessResolveParams struct {
	// SessionID is the chat session the spec is for: the id the host passes to
	// chat.prepare. Required, so a resolution is attributable to a session.
	SessionID string `json:"session_id"`
	// Provider is the session's provider, for the limits that differ by
	// provider (the prompt's token budget, the section names its prompt owns).
	Provider string `json:"provider"`
	// Root is the repository root. Required for kind repo, and when given for
	// any kind it is where the skills listing is read. Validated like every
	// other root: absolute, no symlink, the top of a git work tree.
	Root string `json:"root,omitempty"`
	// Spec says which spec.
	Spec HarnessSpecRef `json:"spec"`
}

// HarnessSkill is one entry of the filtered skills listing: the name and the
// description the prompt's own listing shows, never a body.
type HarnessSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// HarnessRejection is a skill the repository has that was refused, and why. A
// skill that does not appear is reported here, never skipped.
type HarnessRejection struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// HarnessResolveResult is the spec, resolved.
type HarnessResolveResult struct {
	// Sections are the spec's prompt sections in the order the digest covers.
	// The host composes System: its own essentials, then these.
	Sections []harness.Section `json:"sections"`
	// Roles are the sub-agent roles the spec asks for. They are data for a host
	// that runs sub-agents, and can only be narrower than the session.
	Roles []harness.RoleSpec `json:"roles"`
	// Skills are the repository's skills the spec selects (all when it selects
	// none), bounded like the prompt's own listing. Empty when no Root was given.
	Skills []HarnessSkill `json:"skills"`
	// Digest is the spec's digest. A host passes it as chat.prepare's
	// harness_digest.
	Digest string `json:"digest"`
	// Rejected are the skills that were found and refused, with reasons.
	Rejected []HarnessRejection `json:"rejected"`
	// Notes say what is partial about this answer: no Root so no skills were
	// read, skills past a bound, a listing cut to fit. A note is never absent
	// when the answer is less than the whole.
	Notes []string `json:"notes,omitempty"`
}

// HarnessBudget is the token limits of one provider's prompt.
type HarnessBudget struct {
	// Prompt is the whole prompt's budget, zero when unbounded. A spec may take
	// a share of it (harness.AlwaysOnLimit).
	Prompt int
	// Skills is the share of it the skills listing may take, zero when
	// unbounded.
	Skills int
}

// HarnessResolver is what resolving a spec needs from the embedding command.
//
// The reserved names and the budgets come from the command's own prompt
// assembler, never from a list kept in this package: a section added to the
// prompt tomorrow is protected tomorrow, which a copy here would not be.
type HarnessResolver struct {
	// Reserved lists the section and source names the provider's fixed prompt
	// owns, Essential ones included. Required.
	Reserved func(provider string) []string
	// Budget gives the provider's prompt limits. Nil means unbounded.
	Budget func(provider string) HarnessBudget
	// Catalogue holds the named specs. Nil means harness.DefaultCatalogue.
	Catalogue *harness.Catalogue
}

func (h *HarnessResolver) check() error {
	if nilInterface(h) {
		return errors.New("harness module has no resolver")
	}
	if h.Reserved == nil {
		return errors.New("harness module has no reserved-name source: without it a spec could take a section name the prompt already owns")
	}
	return nil
}

func (h *HarnessResolver) budget(provider string) HarnessBudget {
	if h.Budget == nil {
		return HarnessBudget{}
	}
	return h.Budget(provider)
}

func (h *HarnessResolver) catalogue() *harness.Catalogue {
	if h.Catalogue != nil {
		return h.Catalogue
	}
	return harness.DefaultCatalogue
}

// refused is a spec the host must fix. The message carries the whole list.
func harnessRefused(format string, args ...any) *Error {
	return &Error{Code: ErrHarnessRefused, Message: fmt.Sprintf(format, args...)}
}

// resolveSpec finds, strictly decodes and validates one spec. root is needed for
// kind repo only. It never reads a skill and never writes anything.
func (h *HarnessResolver) resolveSpec(ctx context.Context, provider, root string, ref HarnessSpecRef) (harness.Spec, *Error) {
	var spec harness.Spec
	switch ref.Kind {
	case HarnessKindInline:
		if len(bytes.TrimSpace(ref.Inline)) == 0 || bytes.Equal(bytes.TrimSpace(ref.Inline), []byte("null")) {
			return spec, badRequest("harness spec kind inline requires an inline spec document")
		}
		decoded, err := harness.DecodeSpec(ref.Inline)
		if err != nil {
			return spec, harnessRefused("%v", err)
		}
		if ref.Name != "" && ref.Name != decoded.Name {
			return spec, badRequest("harness spec name %q does not match the inline spec's own name %q", ref.Name, decoded.Name)
		}
		spec = decoded
	case HarnessKindRepo, HarnessKindNamed:
		if len(ref.Inline) != 0 {
			return spec, badRequest("harness spec kind %s takes a name, not an inline document", ref.Kind)
		}
		var input harness.Input
		if ref.Kind == HarnessKindRepo {
			if root == "" {
				return spec, badRequest("harness spec kind repo requires a root: the spec is read from %s in that repository", harness.SpecsDir)
			}
			input = harness.RepoSpecs{Read: devcouncil.RepoReaderAt(ctx, root)}
		} else {
			input = h.catalogue()
		}
		found, _, err := harness.Resolve(ref.Name, input)
		var missing *harness.NoSuchSpecError
		switch {
		case errors.As(err, &missing):
			return spec, &Error{Code: ErrHarnessNotFound, Message: err.Error()}
		case ctx.Err() != nil:
			return spec, dependencyError("harness resolve", ctx.Err())
		case err != nil:
			return spec, harnessRefused("%v", err)
		}
		spec = found
	default:
		return spec, badRequest("harness spec kind %q is not one of %s, %s, %s",
			ref.Kind, HarnessKindRepo, HarnessKindNamed, HarnessKindInline)
	}
	env := harness.Env{Provider: provider, Reserved: h.Reserved(provider), BudgetTokens: h.budget(provider).Prompt}
	if problems := spec.Validate(env); len(problems) > 0 {
		lines := make([]string, len(problems))
		for i, p := range problems {
			lines[i] = p.String()
		}
		return spec, harnessRefused("harness spec %q is refused, %d problem(s): %s", spec.Name, len(problems), strings.Join(lines, "; "))
	}
	return spec, nil
}

// resolve answers one harness.resolve.
func (h *HarnessResolver) resolve(ctx context.Context, raw json.RawMessage) (any, *Error) {
	var p HarnessResolveParams
	if e := strictParams(raw, OpHarnessResolve, &p); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.SessionID) == "" {
		return nil, badRequest("harness.resolve requires a session_id")
	}
	if !harness.ValidName(p.Provider) {
		return nil, badRequest("harness.resolve requires a provider name: it must match [a-z0-9][a-z0-9_-]* and be at most 64 characters")
	}
	root := ""
	if p.Root != "" || p.Spec.Kind == HarnessKindRepo {
		var e *Error
		if root, e = canonicalRepoRoot(ctx, "root", p.Root); e != nil {
			return nil, e
		}
	}
	spec, e := h.resolveSpec(ctx, p.Provider, root, p.Spec)
	if e != nil {
		return nil, e
	}
	n := spec.Normalized()
	result := HarnessResolveResult{
		Sections: n.Sections, Roles: n.Roles, Digest: spec.Digest(),
		Skills: []HarnessSkill{}, Rejected: []HarnessRejection{},
	}
	if result.Sections == nil {
		result.Sections = []harness.Section{}
	}
	if result.Roles == nil {
		result.Roles = []harness.RoleSpec{}
	}
	if root == "" {
		result.Notes = append(result.Notes, "no root was given, so the repository's skills were not read and none are listed")
		return result, nil
	}
	if e := h.listSkills(ctx, root, p.Provider, spec, &result); e != nil {
		return nil, e
	}
	return result, nil
}

// listSkills fills the skills listing: the repository's own, through the same
// loader and gated reader the prompt's listing uses, narrowed by the spec and
// then bounded by the budget the prompt's listing is bounded by.
func (h *HarnessResolver) listSkills(ctx context.Context, root, provider string, spec harness.Spec, out *HarnessResolveResult) *Error {
	set, err := harness.LoadSkills(ctx, root, devcouncil.RepoReaderAt(ctx, root))
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		if ctx.Err() != nil {
			return dependencyError("harness resolve", ctx.Err())
		}
		out.Notes = append(out.Notes, fmt.Sprintf("skills could not be read, so none are listed: %v", err))
		return nil
	}
	set = harness.FilterSkills(set, spec.Skills)
	for _, r := range set.Rejected {
		out.Rejected = append(out.Rejected, HarnessRejection{Name: r.Name, Reason: r.Reason})
	}
	if set.RejectedOmitted > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d more skill directories were rejected and are not named here", set.RejectedOmitted))
	}
	if set.NotExamined > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d skill directories past the limit of %d were not examined; the listing is a prefix", set.NotExamined, harness.MaxSkills))
	}
	if set.ListingCut {
		out.Notes = append(out.Notes, fmt.Sprintf("the skills directory holds more than %d entries; only the first %d were considered, so the listing may be missing skills", harness.MaxSkillDirEntries, harness.MaxSkillDirEntries))
	}
	limit := h.budget(provider).Skills
	used := 0
	for i, s := range set.Skills {
		cost := prompt.EstimateTokens(s.Name + " " + s.Description)
		if limit > 0 && used+cost > limit {
			out.Notes = append(out.Notes, fmt.Sprintf("%d of %d skills are not listed: the listing is bounded to %d estimated tokens", len(set.Skills)-i, len(set.Skills), limit))
			break
		}
		used += cost
		out.Skills = append(out.Skills, HarnessSkill{Name: s.Name, Description: s.Description})
	}
	return nil
}

// strictParams decodes an op's params refusing an unknown field. Strictness is
// the safe reading here and not the protocol's default one: a field this build
// does not know could be a widening (a tier, a posture) a host believes was
// applied, and dropping it silently would make the host's belief false.
func strictParams(raw json.RawMessage, op string, into any) *Error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return badRequest("%s params: %v", op, err)
	}
	if dec.More() {
		return badRequest("%s params: trailing data", op)
	}
	return nil
}

// HarnessModule serves harness.resolve.
type HarnessModule struct {
	Resolver *HarnessResolver
}

// Configure implements Module.
func (m HarnessModule) Configure(r *Router) error {
	if err := m.Resolver.check(); err != nil {
		return err
	}
	return r.Register(OpHarnessResolve, m.Resolver.resolve)
}
