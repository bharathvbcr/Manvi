package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// The wire frame: what a conversation sends as `system` and `tools`.
//
// On a provider that binds thinking to the conversation before it, a signed
// block is valid only while the top-level system prompt and the full tool
// definitions are what they were when it was produced
// (shared/model-migration.md, "Breaking change 3": rebuilding either between
// requests of one conversation invalidates every later block). The Loop is
// built per turn and assembles its prompt and tool set per turn, so left alone
// two Loops over one log send different bytes. This file is the one place that
// decides what a conversation sends; the log (session.WireState) remembers it,
// so it survives a resume and is the same for every Loop.
//
//   - system is the prompt of the conversation's first request, forever. A later
//     Loop whose prompt differs delivers it as a mid-conversation role:"system"
//     message after the user turn (shared/prompt-caching.md, "Mid-conversation
//     system messages": no beta header, phrased as context).
//   - tools is the declared set, sorted by name, byte-identical on every
//     request. Narrowing for a step is enforced by the loop (allows) and, when
//     nothing is offered, by tool_choice none; the model is told which tools
//     are available in the same kind of system message. A tool the declared set
//     lacks is a growth of it: the tools sit at the front of the prefix and the
//     documented by-reference form (defer_loading plus tool_addition) needs the
//     tool declared up front, so the frame is re-declared at a turn boundary
//     and the signed reasoning before it is stripped (session.StripStaleReasoning,
//     a leading run, the documented removal). A tool whose definition changed
//     keeps the definition the conversation declared: no append-only form
//     expresses a same-name change without a beta this adapter does not send
//     (causes.md, row tool_schema_changed).
//
// Why the frame opens with the tools the first Loop offers, not the registry's
// full set (ft-9a5710b6 item 1, ruled 2026-11-09: keep). A ChatOnly or ReadOnly
// first turn declares the filtered set, so the first turn that wants more is a
// tools_added growth and strips the reasoning before it. That is the documented
// removal, a leading run, and it is clean (TestAdvChatOnlyFirstTurnThenTools
// StripsCleanlyInsteadOfBreaking). Opening with tools.Registry.Schemas() would
// avoid the strip, but it declares tools the conversation may never be offered,
// puts them in every request's cached prefix, and adds a tools notice for a
// ReadOnly agent on its first turn. The strip costs the reasoning of one narrow
// first turn; the declaration costs every request. Reopen if first turns that
// narrow turn out to carry reasoning worth more than that.
//
// A conversation whose log already holds an assistant message when the frame
// first opens (a log from before frames were recorded, or from a target that
// sent none) has no record of what its blocks were signed under. Its frame is
// recorded as a growth from nothing, so everything before it is stripped rather
// than replayed (see planFrame).
//
// Only where the model accepts a role:"system" message
// (llm.Capability.MidConversationSystem) and the provider binds thinking.
// Everything else keeps sending what the Loop assembled.

// freezes reports whether this Loop sends the conversation's wire frame.
func (l *Loop) freezes() bool {
	if !ServerSideEditing(l.cfg.Provider) {
		return false
	}
	c, ok := l.cfg.Provider.Capability(l.cfg.Model)
	return ok && c.MidConversationSystem
}

// settleFrame records the conversation's frame when it has none, grows it at a
// turn boundary when the Loop wants a tool it does not declare, and holds the
// result on the Loop for the turn. It runs before the turn's system/prompt
// event, because the frame of a conversation that predates it is the prompt that
// event last recorded: that is what was sent, and its blocks were signed under
// it.
func (l *Loop) settleFrame() error {
	l.wire = session.WireState{}
	if !l.freezes() {
		return nil
	}
	next, err := l.planFrame()
	if err != nil {
		return err
	}
	if next != nil {
		if _, err := l.log.Append(session.WireFrame, *next); err != nil {
			return err
		}
	}
	l.wire = l.log.Wire()
	return nil
}

// planFrame is the one rule for what settleFrame records: the frame event to
// append, or nil when the log's frame already holds (or, mid tool round, must
// wait). It records nothing, so Draft asks it too and sizes the request from the
// frame the turn will send.
//
//   - No frame, no assistant message yet: the frame opens with the prompt and the
//     tools the Loop wants.
//   - No frame, but an assistant message already in the log: the conversation
//     predates the frame, and which system prompt and tools its signed blocks were
//     produced under was never recorded. The frame is recorded as a growth of the
//     tool set from nothing, so it is a boundary: the reasoning before it is
//     stripped (a leading run, session.StripStaleReasoning) instead of being
//     replayed behind a prefix nobody can vouch for. Mid tool round it waits for
//     the turn to end, as a growth does.
//   - A frame, and wanted tools it lacks: a growth, at a turn boundary.
func (l *Loop) planFrame() (*session.FrameData, error) {
	state := l.log.Wire()
	wanted := l.schemas()
	if state.Frame != nil {
		return l.growFrame(state.Frame, wanted)
	}
	history, err := l.log.DeriveMessages()
	if err != nil {
		return nil, err
	}
	system := l.log.SystemPrompt()
	if system == "" {
		system = l.cfg.SystemPrompt
	}
	if !slices.ContainsFunc(history, func(m llm.Message) bool { return m.Role == llm.RoleAssistant }) {
		return &session.FrameData{System: system, Tools: sortedTools(wanted), Why: session.WhyOpened}, nil
	}
	if !AtTurnBoundary(history) {
		return nil, nil
	}
	return &session.FrameData{
		System: system, Tools: sortedTools(wanted), Why: session.WhyToolsAdded, Added: toolNames(wanted),
	}, nil
}

// growFrame is the frame that re-declares frame with the wanted tools it lacks,
// at a turn boundary; nil when there is nothing to add or the turn is not over.
func (l *Loop) growFrame(frame *session.FrameData, wanted []llm.ToolSchema) (*session.FrameData, error) {
	added := undeclared(frame.Tools, wanted)
	if len(added) == 0 {
		return nil, nil
	}
	history, err := l.log.DeriveMessages()
	if err != nil {
		return nil, err
	}
	// Mid tool round the assistant turn awaiting its result keeps its signed
	// reasoning, so the growth waits for the turn to end.
	if !AtTurnBoundary(history) {
		return nil, nil
	}
	return &session.FrameData{
		System: frame.System,
		Tools:  sortedTools(append(slices.Clone(frame.Tools), added...)),
		Why:    session.WhyToolsAdded, Added: toolNames(added),
	}, nil
}

// toolNames is the names of schemas, sorted.
func toolNames(schemas []llm.ToolSchema) []string {
	names := make([]string, len(schemas))
	for i, s := range schemas {
		names[i] = s.Name
	}
	slices.Sort(names)
	return names
}

// draftFrame is the system prompt and tools the next request of this
// conversation will carry where the frame is sent: the log's frame, as the turn's
// settleFrame would leave it. ok is false where no frame is sent (or none can be
// planned), and the caller sizes from the Loop's own prompt and tools.
func (l *Loop) draftFrame() (system string, tools []llm.ToolSchema, ok bool) {
	if !l.freezes() {
		return "", nil, false
	}
	next, err := l.planFrame()
	if err != nil {
		return "", nil, false
	}
	if next == nil {
		next = l.log.Wire().Frame
	}
	if next == nil {
		return "", nil, false
	}
	return next.System, next.Tools, true
}

// frozenFrame is the conversation's frame when this Loop sends it.
func (l *Loop) frozenFrame() *session.FrameData {
	if l.wire.Frame == nil || !l.freezes() {
		return nil
	}
	return l.wire.Frame
}

// wireSchemas is the tool set the request will carry, which is what server-side
// clearing and the size estimate must be asked about.
func (l *Loop) wireSchemas() []llm.ToolSchema {
	if f := l.frozenFrame(); f != nil {
		return f.Tools
	}
	return l.schemas()
}

// declares reports whether name is in the conversation's declared tool set. A
// Loop that sends no frame declares nothing of its own, so it allows everything.
func (l *Loop) declares(name string) bool {
	f := l.frozenFrame()
	if f == nil {
		return true
	}
	return slices.ContainsFunc(f.Tools, func(s llm.ToolSchema) bool { return s.Name == name })
}

// freezeRequest puts the conversation's frame on a shaped request and returns
// the names the request really offers: the shaped tools the frame declares. A
// notice is recorded and appended when the model's instructions or available
// tools differ from what it was last told, as one message so two system
// messages never sit side by side.
//
// What goes out is what the log holds. The frame is read back from the log, and
// the notice that is sent is the one Append returned, so a scrubber that rewrote
// the prompt rewrote the message the model sees too; and the prompt is compared
// with the one the log recorded in its scrubbed form, so a scrubbed conversation
// does not look changed on every turn.
//
// The prompt is the shaped request's, not Config.SystemPrompt: a listener that
// edited System in the LLMRequest waterfall changed what the model should be
// told, and top-level system cannot carry it, so it is delivered like any other
// prompt change. The reason to deliver rather than refuse is that the edit is
// the listener's intent and the notice is the append-only form of it.
func (l *Loop) freezeRequest(request llm.Request) (llm.Request, map[string]struct{}, error) {
	frame := l.frozenFrame()
	if frame == nil {
		return request, schemaNames(request.Tools), nil
	}
	declared := make([]string, len(frame.Tools))
	for i, s := range frame.Tools {
		declared[i] = s.Name
	}
	var offered []string
	for _, s := range request.Tools {
		if slices.Contains(declared, s.Name) {
			offered = append(offered, s.Name)
		}
	}
	slices.Sort(offered)
	offered = slices.Compact(offered)

	prompt := request.System
	request.System = frame.System
	request.Tools = frame.Tools
	request.NoToolCalls = len(offered) == 0

	// The state is read from the log: a notice that went unanswered since this
	// Loop last looked is no longer delivered.
	l.wire = l.log.Wire()
	told := l.wire.Told
	if told == nil {
		told = declared
	}
	var data session.NoticeData
	var parts []string
	if prompt != "" && l.log.Scrubbed(prompt) != l.wire.Delivered {
		data.Prompt = prompt
		parts = append(parts, systemNoticeText(prompt))
	}
	if len(offered) > 0 && !slices.Equal(offered, told) {
		data.Offered = offered
		parts = append(parts, toolsNoticeText(offered, declared))
	}
	names := make(map[string]struct{}, len(offered))
	for _, n := range offered {
		names[n] = struct{}{}
	}
	if len(parts) == 0 || !endsWithUser(request.Messages) {
		return request, names, nil
	}
	data.Text = strings.Join(parts, "\n\n")
	event, err := l.log.Append(session.WireNotice, data)
	if err != nil {
		return llm.Request{}, nil, err
	}
	var logged session.NoticeData
	if err := json.Unmarshal(event.Data, &logged); err != nil {
		return llm.Request{}, nil, fmt.Errorf("agent: reading back the notice just logged: %w", err)
	}
	l.wire = l.log.Wire()
	request.Messages = append(slices.Clone(request.Messages), session.NoticeMessage(logged))
	return request, names, nil
}

// endsWithUser reports whether a system message may follow messages: it must
// follow a user message (shared/prompt-caching.md, "Mid-conversation system
// messages").
func endsWithUser(messages []llm.Message) bool {
	return len(messages) > 0 && messages[len(messages)-1].Role == llm.RoleUser
}

// systemNoticeText delivers a changed system prompt in full, as context.
func systemNoticeText(prompt string) string {
	return "The instructions for this conversation have been updated. The current " +
		"instructions follow in full; where they differ from the ones given earlier, " +
		"these are the ones that apply from here on.\n\n" + prompt
}

// toolsNoticeText says which of the declared tools are available.
func toolsNoticeText(offered, declared []string) string {
	if len(offered) == len(declared) {
		return "All of the tools declared for this conversation are available again."
	}
	return "The tools available from here on are: " + strings.Join(offered, ", ") +
		". The other declared tools are not available until this changes."
}

// sortedTools returns schemas ordered by name, so the tools a conversation
// sends are the same bytes on every request.
func sortedTools(schemas []llm.ToolSchema) []llm.ToolSchema {
	out := slices.Clone(schemas)
	slices.SortStableFunc(out, func(a, b llm.ToolSchema) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// undeclared returns the wanted tools whose names the declared set lacks.
func undeclared(declared, wanted []llm.ToolSchema) []llm.ToolSchema {
	var out []llm.ToolSchema
	for _, w := range wanted {
		if !slices.ContainsFunc(declared, func(d llm.ToolSchema) bool { return d.Name == w.Name }) {
			out = append(out, w)
		}
	}
	return out
}

// withoutSystemMessages returns messages without the mid-conversation system
// messages, sharing the slice when there are none.
func withoutSystemMessages(messages []llm.Message) []llm.Message {
	if !slices.ContainsFunc(messages, func(m llm.Message) bool { return m.Role == llm.RoleSystem }) {
		return messages
	}
	return slices.DeleteFunc(slices.Clone(messages), func(m llm.Message) bool { return m.Role == llm.RoleSystem })
}
