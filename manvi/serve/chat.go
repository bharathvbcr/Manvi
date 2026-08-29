package serve

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"manvi/agent"
	"manvi/llm"
	"manvi/llm/openaicompat"
)

// The chat plane is advisory: it does not make the model call.
//
// That is a deliberate split. A host that already drives its own HTTP client
// has cancellation, progress events, image handling and tool dispatch wired
// into its own UI, and moving the request here would mean either rebuilding
// all of that in Go or inventing a bidirectional streaming protocol to hand it
// back. What the host cannot easily get is the part that needs *memory across
// steps* and *knowledge of local-server behaviour*: which tool results have
// already been shortened, how far the token estimate is off for this server,
// and how to read a reply from a model whose server did not parse it.
//
// So there are two calls, around the host's own request:
//
//	chat.prepare  — before it: what to shorten, and by how much
//	chat.settle   — after it:  what the reply actually contained
//
// Together they are one step of a turn, which is why they share a session.
//
// What this plane therefore does NOT provide, stated because the absence is
// otherwise invisible: agent.Loop does not run here, so the terminal checkpoint
// does not fire, and the harness's own end-of-turn check never runs. The host
// owns turn completion. Every guarantee the two faces get from that
// checkpoint — a mutating turn is verified before it closes, a failed check
// bounces the model once with the findings, a check that could not run is
// recorded as degraded rather than as a pass — is the host's responsibility on
// this plane and not this package's.
//
// It is a scope boundary rather than a gap to close. Running a checkpoint here
// would mean this package deciding a turn is over, which is precisely the
// decision the split above hands to the host. A host that wants the guarantee
// runs the turn through `manvi run` instead.

// ChatSession is the state that must survive between steps.
type ChatSession struct {
	// compacted names every tool result that already carries a shortened
	// form. It is the reason compaction is one-way, and one-way is the reason
	// a local server's prefix cache survives a turn: the KV cache is keyed on
	// an unchanged token prefix, so re-shortening a result to a different
	// string invalidates everything after it and costs a full re-prefill —
	// measured at 120s for a 14.7k-token prompt on a 4-bit 27B against 1.5s
	// warm.
	compacted map[llm.CallID]string
	// calibrator corrects the token estimator against what the server counts.
	calibrator agent.Calibrator
	// lastEstimate is what prepare predicted for the request the host then
	// sent, held so the next call can pair it with the server's real count.
	lastEstimate int
	// lastUsed is when this session was last touched, driving idle eviction.
	// A host that crashes — or simply abandons a tab without forget — leaves
	// its ledger here; without a clock this table only ever grows.
	lastUsed time.Time
}

func newChatSession() *ChatSession {
	return &ChatSession{compacted: map[llm.CallID]string{}, lastUsed: time.Now()}
}

// pruneLedger drops what the ledger no longer needs to remember, given the
// history the host has just sent.
//
// A ledger entry earns its place by substituting a shortened result back into
// the next request (toNeutralMessages) and by keeping that result off the next
// plan (the `already` set). An entry for a tool_call_id the host no longer
// sends does neither: the conversation moved on, the result is gone, and the
// entry is pure residue. Dropping it is therefore free — it cannot move a
// prompt prefix that no longer contains the message.
//
// What is left is bounded by what the host sends, and one request is bounded
// by maxLineBytes; maxCompactedEntries is the second bound, for a host whose
// history itself keeps growing. That one is not free — a dropped entry gets
// re-shortened, and possibly to different text — so it takes the front of the
// history, which is the part furthest from the protected tail the model is
// actively reasoning over.
func (s *ChatSession) pruneLedger(wire []WireMessage) {
	if len(s.compacted) == 0 {
		return
	}
	live := make(map[llm.CallID]struct{}, len(wire))
	order := make([]llm.CallID, 0, len(wire))
	for _, m := range wire {
		if m.Role != "tool" || m.ToolCallID == "" {
			continue
		}
		id := llm.CallID(m.ToolCallID)
		if _, seen := live[id]; seen {
			continue
		}
		live[id] = struct{}{}
		order = append(order, id)
	}
	for id := range s.compacted {
		if _, ok := live[id]; !ok {
			delete(s.compacted, id)
		}
	}
	for i := 0; len(s.compacted) > maxCompactedEntries && i < len(order); i++ {
		delete(s.compacted, order[i])
	}
}

// Session-table bounds.
//
// The table exists so compaction is one-way within a conversation; nothing
// about that requires remembering every conversation forever. A host that
// calls chat.forget keeps its sessions precisely; these bounds are what
// happens when it does not — a crashed host, an abandoned tab, or a hostile
// caller minting ids to see what happens. Every bound here costs at most one
// re-planning of what it drops, which is exactly what an absent sidecar costs
// anyway.
const (
	// maxChatSessions caps the table outright.
	maxChatSessions = 256
	// chatSessionIdleTTL drops sessions unused for this long. Longer than any
	// turn a real user sits through; short enough that a crashed host's
	// ledgers are gone within the hour instead of at process exit.
	chatSessionIdleTTL = time.Hour
	// maxCompactedEntries caps *one* session's ledger.
	//
	// The two bounds above cap how many ledgers exist and never touched how
	// large one grows, and the ledger is append-only: every plan adds an entry
	// and nothing but chat.forget ever removed one. A single session driven
	// with fresh tool_call_ids therefore grew without limit — measured at
	// exactly linear, 2,000 entries and 621 KB after 40 requests, with no knee
	// to stop at. The comment that used to sit on maxChatSessions ("hundreds
	// of them are megabytes at worst") was true of an honest host and of
	// nothing else, which is the wrong thing for a bound to be true of.
	//
	// Pruning to what the host still sends (see pruneLedger) is what usually
	// holds the size down; this is the number for the case where the host's
	// own history is what grew. It is far above any real conversation — a turn
	// with 4,096 outstanding compacted tool results has other problems — so
	// crossing it is a host defect, and the cost of crossing it is that the
	// oldest results get shortened again.
	maxCompactedEntries = 4096
)

func (t *sessionTable) get(id string) *ChatSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byID == nil {
		t.byID = map[string]*ChatSession{}
	}
	s, ok := t.byID[id]
	if !ok {
		s = newChatSession()
		t.byID[id] = s
		t.evictLocked(id)
	}
	s.lastUsed = time.Now()
	return s
}

// evictLocked enforces the table's bounds. Caller holds mu; justCreated names
// the session that must survive this call even if it is somehow the oldest.
//
// Two passes, both cheap at this table size: everything past the idle TTL
// goes first (age is the fairer reason to drop), then LRU down to the cap.
func (t *sessionTable) evictLocked(justCreated string) {
	now := time.Now()
	for id, s := range t.byID {
		if id != justCreated && now.Sub(s.lastUsed) > chatSessionIdleTTL {
			delete(t.byID, id)
		}
	}
	for len(t.byID) > maxChatSessions {
		oldestID := ""
		var oldest time.Time
		for id, s := range t.byID {
			if id == justCreated {
				continue
			}
			if oldestID == "" || s.lastUsed.Before(oldest) {
				oldestID = id
				oldest = s.lastUsed
			}
		}
		if oldestID == "" {
			// Only justCreated remains; the cap cannot apply to it.
			break
		}
		delete(t.byID, oldestID)
	}
}

// sessions holds per-conversation chat state.
//
// Keyed by a host-chosen id — in practice a tab or conversation — because two
// conversations against the same model have unrelated histories and sharing a
// compaction ledger between them would elide a result one of them never saw.
type sessionTable struct {
	mu   sync.Mutex
	byID map[string]*ChatSession
}

func (t *sessionTable) drop(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byID, id)
}

// WireMessage is one conversation entry in the host's terms.
//
// It is deliberately not manvi's llm.Message: a host should not have to
// construct a neutral content-block tree to ask what to compact. Everything
// the compaction planner needs is a role, the visible text, and — for a tool
// result — the id that pairs it with its call.
type WireMessage struct {
	// Role is "user", "assistant", or "tool".
	Role string `json:"role"`
	// Text is the visible content.
	Text string `json:"text,omitempty"`
	// ToolCallID pairs a tool result with the call that produced it. Required
	// on a tool message: without it the result cannot be named in a plan, so
	// it can never be compacted.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls are the calls an assistant message asked for. Counted toward
	// the budget; never compacted, because shortening a call changes what the
	// tool was asked to do.
	ToolCalls []WireToolCall `json:"tool_calls,omitempty"`
}

// WireToolCall is one call in an assistant message.
type WireToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// WireTool is one tool offered to the model.
//
// Schemas are counted in the budget rather than ignored: they go out on every
// request and are not small — manvi's own surface measures 1,755 real tokens —
// so omitting them understates every budget in the direction that overflows.
type WireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// PrepareParams asks what to shorten before a request goes out.
type PrepareParams struct {
	// SessionID scopes the compaction ledger and the calibrator. Required:
	// without it every step would start with an empty ledger and re-shorten
	// results it had already shortened, which is the exact prefix churn this
	// whole plane exists to avoid.
	SessionID string `json:"session_id"`
	// System is the assembled system prompt.
	System string `json:"system,omitempty"`
	// Tools are the schemas that will be offered.
	Tools []WireTool `json:"tools,omitempty"`
	// Messages is the history as it stands.
	Messages []WireMessage `json:"messages"`

	// ContextWindow is the model's total token capacity — ideally the value
	// capability.probe discovered rather than a default. Must be positive and
	// no larger than maxTokenCount.
	ContextWindow int `json:"context_window"`
	// ReservedOutput is held back for the response. Zero means a default
	// proportional to the window. Bounded like ContextWindow.
	ReservedOutput int `json:"reserved_output,omitempty"`
	// Overhead covers chat-template scaffolding the estimator does not model.
	// Bounded like ContextWindow.
	Overhead int `json:"overhead,omitempty"`

	// ObservedPromptTokens is what the server reported for the *previous*
	// request in this session (Ollama's prompt_eval_count, or usage
	// .prompt_tokens). It is how the estimator corrects itself: the estimator
	// runs high — around 25% against a real Qwen tokenizer, 58% on the JSON
	// that tool results are made of — and an uncorrected estimate compacts a
	// model down to a fraction of the context it actually had.
	ObservedPromptTokens int `json:"observed_prompt_tokens,omitempty"`
}

// PrepareStep is one tool result to shorten, and the text to shorten it to.
type PrepareStep struct {
	ToolCallID string `json:"tool_call_id"`
	Text       string `json:"text"`
	FromBytes  int    `json:"from_bytes"`
	ToBytes    int    `json:"to_bytes"`
}

// PrepareResult is the plan.
type PrepareResult struct {
	// Steps are the replacements to apply, in no particular order. A host
	// applies them by tool_call_id and must persist the result: the same text
	// has to go out on every later step, or the prefix moves anyway and the
	// plan achieved nothing.
	Steps []PrepareStep `json:"steps"`
	// Before and After are calibrated token estimates of the whole request.
	Before int `json:"before_tokens"`
	After  int `json:"after_tokens"`
	// Threshold is where compaction triggers; Target is what it aims for.
	//
	// They differ on purpose. Compacting to exactly the threshold means the
	// next tool result crosses it again and the prefix moves again, so the
	// target sits well under it and makes compaction a rare event — which is
	// the only thing that makes it affordable.
	Threshold int `json:"threshold_tokens"`
	Target    int `json:"target_tokens"`
	// Insufficient reports that every eligible result was shortened as far as
	// it goes and the history still exceeds the threshold. Surfaced rather
	// than swallowed: the request is now going to overflow the server's
	// window, and a host that carried on silently would produce a truncation
	// nobody could explain.
	Insufficient bool `json:"insufficient"`
	// CalibrationRatio is the correction currently applied to the estimate,
	// and CalibrationSamples how many server counts back it. One sample is a
	// guess with a number attached; the host can say which it has.
	CalibrationRatio   float64 `json:"calibration_ratio"`
	CalibrationSamples int     `json:"calibration_samples"`
}

// prepare answers OpChatPrepare.
func (s *Server) prepare(raw json.RawMessage) (any, *Error) {
	var p PrepareParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, badRequest("chat.prepare params: %v", err)
	}
	if strings.TrimSpace(p.SessionID) == "" {
		return nil, badRequest(
			"chat.prepare requires a session_id; without one the compaction ledger " +
				"resets every step and re-shortens results it already shortened")
	}
	if p.ContextWindow <= 0 {
		return nil, badRequest("chat.prepare requires a positive context_window")
	}
	for _, bound := range []struct {
		field string
		value int
	}{
		{"context_window", p.ContextWindow},
		{"reserved_output", p.ReservedOutput},
		{"overhead", p.Overhead},
		{"observed_prompt_tokens", p.ObservedPromptTokens},
	} {
		if err := checkTokenCount(bound.field, bound.value); err != nil {
			return nil, err
		}
	}

	session := s.chat.get(p.SessionID)

	// Drop what this history no longer references before anything reads the
	// ledger, so both the substitution below and the `already` set built from
	// it describe the request the host actually sent.
	session.pruneLedger(p.Messages)

	// Fold in what the server counted for the previous request before
	// planning this one, so the correction applies to the decision it informs
	// rather than to the one after it.
	if p.ObservedPromptTokens > 0 && session.lastEstimate > 0 {
		session.calibrator.Observe(session.lastEstimate, p.ObservedPromptTokens)
	}

	messages := toNeutralMessages(p.Messages, session.compacted)
	tools := toNeutralTools(p.Tools)

	reserved := p.ReservedOutput
	if reserved <= 0 {
		// A quarter of the window, bounded. Too small and a long answer is
		// truncated; too large and history is compacted to buy space no reply
		// will use.
		reserved = p.ContextWindow / 4
		if reserved > 4096 {
			reserved = 4096
		}
	}
	budget := agent.Budget{
		ContextWindow:  p.ContextWindow,
		ReservedOutput: reserved,
		Overhead:       p.Overhead,
	}

	already := make(map[llm.CallID]struct{}, len(session.compacted))
	for id := range session.compacted {
		already[id] = struct{}{}
	}

	plan := agent.PlanCompactionCalibrated(
		messages, p.System, tools, budget, already, &session.calibrator)

	result := PrepareResult{
		Before:             plan.Before,
		After:              plan.After,
		Threshold:          budget.Threshold(),
		Target:             budget.Target(),
		Insufficient:       plan.Insufficient,
		CalibrationRatio:   session.calibrator.Ratio(),
		CalibrationSamples: session.calibrator.Samples(),
		Steps:              make([]PrepareStep, 0, len(plan.Steps)),
	}
	for _, step := range plan.Steps {
		// Recorded here, not only returned. The ledger is what makes the next
		// step skip this result instead of recomputing a possibly different
		// shortening for it.
		session.compacted[step.ToolCallID] = step.Text
		result.Steps = append(result.Steps, PrepareStep{
			ToolCallID: string(step.ToolCallID),
			Text:       step.Text,
			FromBytes:  step.FromBytes,
			ToBytes:    step.ToBytes,
		})
	}

	// What the host is about to send, so the next call can pair it with the
	// server's count. After the plan, because the plan is what changes it.
	session.lastEstimate = plan.After
	return result, nil
}

// SettleParams asks what a finished reply actually contained.
type SettleParams struct {
	// Content is the assistant text exactly as the server produced it,
	// including any think tags or tool-call markup it did not parse.
	Content string `json:"content"`
	// Tools are the schemas that were offered. Required for recovery: argument
	// types are taken from the declared schema rather than guessed, because
	// the XML spelling carries no types and guessing turns "0755" into 755.
	Tools []WireTool `json:"tools,omitempty"`
	// ServerParsedCalls reports that the server already returned structured
	// tool calls. Recovery is then skipped entirely — text that merely looks
	// like a call, in a reply whose real calls were parsed, is prose.
	ServerParsedCalls bool `json:"server_parsed_calls,omitempty"`
	// AssumePrefill declares that the chat template ends the prompt with an
	// open thinking tag, as Qwen3's does.
	AssumePrefill bool `json:"assume_prefill,omitempty"`
	// ReasoningOutOfBand reports that the server delivered reasoning on its own
	// channel — an OpenAI-compatible "reasoning_content" or "reasoning" field —
	// rather than inline in the content.
	//
	// It disproves AssumePrefill, and the host is the only one who can see it:
	// this op receives Content alone. A server that separates the two itself is
	// not prefilling a tag into the content stream, so what arrives here is
	// already the answer. Believing the declaration instead files the whole
	// answer as reasoning and returns an empty Text — measured against omlx
	// 0.6.2 serving Qwen3.8-27B, where every text answer was lost this way.
	ReasoningOutOfBand bool `json:"reasoning_out_of_band,omitempty"`
	// OutputTokens and MaxTokensApplied let truncation be checked rather than
	// taken on the server's word. FinishReason is a claim, and servers get it
	// wrong: three responses measured against omlx generated exactly the
	// requested 16,384 tokens and every one was reported as "stop". Zero for
	// either means the check cannot run and FinishReason stands alone.
	OutputTokens     int `json:"output_tokens,omitempty"`
	MaxTokensApplied int `json:"max_tokens_applied,omitempty"`
	// FinishReason is the server's own word for why generation stopped
	// ("stop", "length", "tool_calls", …). "length" is the one that matters.
	FinishReason string `json:"finish_reason,omitempty"`
}

// SettleResult is the reply, read.
type SettleResult struct {
	// Text is the visible answer, with think tags and any recovered call
	// markup removed.
	Text string `json:"text"`
	// Reasoning is thinking separated out of the text, so it is not stored as
	// the answer and replayed on every later step.
	Reasoning string `json:"reasoning,omitempty"`
	// PrefillDisproved reports that AssumePrefill was set and the server
	// contradicted it by delivering reasoning out of band. The declaration was
	// dropped for this reply.
	//
	// Surfaced rather than absorbed: a host that silently compensates forever
	// never gets the setting corrected, and left set against a server that
	// separates reasoning the declaration files every answer as reasoning and
	// returns nothing.
	PrefillDisproved bool `json:"prefill_disproved,omitempty"`
	// Calls are tool calls recovered from text the server did not parse.
	Calls []openaicompat.RecoveredCall `json:"calls,omitempty"`
	// Format names the spelling recovery recognised, empty when none was
	// needed. A host should surface a non-empty value: it means the server is
	// running without a tool parser for the model it serves.
	Format string `json:"format,omitempty"`
	// Reclassified reports that text already streamed to a user turned out to
	// be reasoning, because a closing think tag arrived with nothing open.
	Reclassified bool `json:"reclassified,omitempty"`

	// Truncated reports that generation hit the output cap.
	Truncated bool `json:"truncated,omitempty"`
	// TruncatedMidCall reports the dangerous form: the cap landed inside a
	// tool call's arguments.
	//
	// Such a call must never be dispatched — half an argument object is not a
	// smaller request, it is a different one. But the turn must not fail
	// either: local servers truncate readily (mlx-vlm defaults to 2048), and
	// discarding every completed step over it is a far worse outcome than
	// asking the model to try again. Retry is the message below.
	TruncatedMidCall bool `json:"truncated_mid_call,omitempty"`
	// RetryMessage is what to hand back to the model as a tool-style error
	// when TruncatedMidCall is set. Empty otherwise.
	RetryMessage string `json:"retry_message,omitempty"`
}

// settle answers OpChatSettle.
func (s *Server) settle(raw json.RawMessage) (any, *Error) {
	var p SettleParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, badRequest("chat.settle params: %v", err)
	}

	// The server's word, cross-checked against its own numbers where the host
	// supplied them. Same rule as the streaming path's hitOutputCap.
	truncated := p.FinishReason == "length" ||
		(p.MaxTokensApplied > 0 && p.OutputTokens >= p.MaxTokensApplied)

	// Wire evidence beats the declaration, exactly as it does in the streaming
	// filter. Passing the resolved value keeps one rule in one place: the
	// recovery helpers stay a pure function of what they are told.
	assumePrefill := p.AssumePrefill && !p.ReasoningOutOfBand
	prefillDisproved := p.AssumePrefill && p.ReasoningOutOfBand

	// Reasoning is separated whether or not calls were parsed: a server that
	// returns structured calls can still leave think tags in the content.
	if p.ServerParsedCalls {
		visible, reasoning, reclassified := openaicompat.SplitReasoning(p.Content, assumePrefill)
		return SettleResult{
			Text:             visible,
			Reasoning:        reasoning,
			Reclassified:     reclassified,
			Truncated:        truncated,
			PrefillDisproved: prefillDisproved,
		}, nil
	}

	recovery := openaicompat.RecoverFromText(p.Content, toNeutralTools(p.Tools), assumePrefill)
	out := SettleResult{
		Text:             recovery.Text,
		Reasoning:        recovery.Reasoning,
		Calls:            recovery.Calls,
		Format:           recovery.Format,
		Reclassified:     recovery.Reclassified,
		Truncated:        truncated,
		PrefillDisproved: prefillDisproved,
	}

	// A cap that landed inside call markup: the opening tag is still in the
	// text because the parser refused to close it, and no call came out.
	if truncated && len(recovery.Calls) == 0 && looksLikeUnterminatedCall(recovery.Text) {
		out.TruncatedMidCall = true
		out.RetryMessage = "Your previous message hit the output limit partway through a tool " +
			"call, so it was not run. Nothing was executed and no work was lost. Reissue just " +
			"that call, on its own, with shorter arguments."
	}
	return out, nil
}

// looksLikeUnterminatedCall reports call markup that was opened and never
// closed, which is what a response cut off mid-arguments leaves behind.
func looksLikeUnterminatedCall(text string) bool {
	for _, open := range []string{"<tool_call>", "<function="} {
		if idx := strings.LastIndex(text, open); idx >= 0 {
			closer := "</tool_call>"
			if open == "<function=" {
				closer = "</function>"
			}
			if !strings.Contains(text[idx:], closer) {
				return true
			}
		}
	}
	return false
}

// ForgetParams drops a session's chat state.
type ForgetParams struct {
	SessionID string `json:"session_id"`
}

// forget answers OpChatForget, for a host starting a new conversation.
//
// It exists because the alternative is a table that only grows: a long-running
// desktop app opens and closes conversations all day, and a compaction ledger
// per closed tab is a leak with a slow fuse.
func (s *Server) forget(raw json.RawMessage) (any, *Error) {
	var p ForgetParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, badRequest("chat.forget params: %v", err)
	}
	if p.SessionID == "" {
		return nil, badRequest("chat.forget requires a session_id")
	}
	s.chat.drop(p.SessionID)
	return map[string]bool{"forgotten": true}, nil
}

// toNeutralMessages converts the wire shape to what the planner reads,
// substituting any shortening already recorded for a tool result.
//
// The substitution is what makes the accounting honest: without it the planner
// would price a result at its original length, decide the history is over
// budget, and find nothing left to shorten.
func toNeutralMessages(wire []WireMessage, compacted map[llm.CallID]string) []llm.Message {
	out := make([]llm.Message, 0, len(wire))
	for _, m := range wire {
		switch m.Role {
		case "tool":
			id := llm.CallID(m.ToolCallID)
			text := m.Text
			if short, ok := compacted[id]; ok {
				text = short
			}
			out = append(out, llm.Message{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{llm.ToolResultBlock{
					ToolCallID: id,
					Content:    []llm.ContentBlock{llm.TextBlock{Text: text}},
				}},
			})
		case "assistant":
			blocks := make([]llm.ContentBlock, 0, 1+len(m.ToolCalls))
			if m.Text != "" {
				blocks = append(blocks, llm.TextBlock{Text: m.Text})
			}
			for _, c := range m.ToolCalls {
				args := json.RawMessage(c.Arguments)
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				blocks = append(blocks, llm.ToolCallBlock{
					ID: llm.CallID(c.ID), Name: c.Name, Arguments: args,
				})
			}
			out = append(out, llm.Message{Role: llm.RoleAssistant, Content: blocks})
		default:
			out = append(out, llm.Message{
				Role:    llm.RoleUser,
				Content: []llm.ContentBlock{llm.TextBlock{Text: m.Text}},
			})
		}
	}
	return out
}

func toNeutralTools(wire []WireTool) []llm.ToolSchema {
	out := make([]llm.ToolSchema, 0, len(wire))
	for _, t := range wire {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage("{}")
		}
		out = append(out, llm.ToolSchema{
			Name: t.Name, Description: t.Description, InputSchema: schema,
		})
	}
	return out
}
