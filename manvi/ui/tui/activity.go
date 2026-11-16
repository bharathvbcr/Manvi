package tui

import (
	"encoding/json"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// toolAct is how a tool call reads in the transcript: what it is doing while it
// runs, what it did once it has, and which of its arguments it did it to.
//
// A call was drawn as the tool's own name and a list of its arguments,
// "devcouncil_read_file  path=a.go  limit=120": the vocabulary of the tool
// catalogue, in a place a person reads to follow the work. "Read a.go" is the
// same fact.
type toolAct struct {
	doing, done string
	// subject is the argument the call is about. Empty means the first of
	// subjectKeys the call carries.
	subject string
	// where is a second argument that narrows the subject, drawn as "in …".
	where string
	// list is an argument holding several things the call is about — the
	// subagents it starts, the tools it enables — and noun what one of them is
	// called. The row counts them and names the first few.
	list, noun string
	// object is what the call is about when no argument says: "the task", for a
	// call that verifies whichever task is checked out.
	object string
}

// toolActs is keyed by tool name without the catalogue's prefix. A tool that is
// not here — one an MCP server brought, or one added since — is "Using" its own
// name, with its arguments.
var toolActs = map[string]toolAct{
	"read_file":    {doing: "Reading", done: "Read", subject: "path"},
	"write_file":   {doing: "Writing", done: "Wrote", subject: "path"},
	"patch_file":   {doing: "Editing", done: "Edited", subject: "path"},
	"delete_file":  {doing: "Deleting", done: "Deleted", subject: "path"},
	"list_dir":     {doing: "Listing", done: "Listed", subject: "path"},
	"grep":         {doing: "Searching for", done: "Searched for", subject: "pattern", where: "path"},
	"find_files":   {doing: "Looking for", done: "Looked for", subject: "pattern", where: "path"},
	"exec_command": {doing: "Running", done: "Ran", subject: "command"},
	"fetch_url":    {doing: "Fetching", done: "Fetched", subject: "url"},
	"get_diff":     {doing: "Reading", done: "Read", object: "the diff"},

	// Other agents, and what this one may use.
	"spawn_subagents":  {doing: "Starting", done: "Ran", list: "tasks", noun: "subagent"},
	"invoke_subagent":  {doing: "Starting", done: "Ran", list: "subagents", noun: "subagent"},
	"define_subagent":  {doing: "Defining subagent", done: "Defined subagent", subject: "name"},
	"manage_subagents": {doing: "Managing subagents:", done: "Managed subagents:", subject: "action"},
	"send_message":     {doing: "Messaging", done: "Messaged", subject: "recipient"},
	"load_skill":       {doing: "Loading skill", done: "Loaded skill", subject: "name"},
	"search_tools":     {doing: "Looking up tools for", done: "Looked up tools for", subject: "query"},
	"activate_tools":   {doing: "Enabling", done: "Enabled", list: "tools", noun: "tool"},
	"ask_question":     {doing: "Asking", done: "Asked", list: "questions", noun: "question"},

	// The task board and the gate.
	"next_task":        {doing: "Picking", done: "Picked", object: "the next task"},
	"get_task":         {doing: "Reading task", done: "Read task", subject: "task_id"},
	"checkout_task":    {doing: "Checking out", done: "Checked out", subject: "task_id"},
	"release_task":     {doing: "Releasing", done: "Released", subject: "task_id", object: "the task"},
	"renew_lease":      {doing: "Renewing", done: "Renewed", object: "the lease"},
	"verify_task":      {doing: "Verifying", done: "Verified", subject: "task_id", object: "the task"},
	"request_override": {doing: "Requesting", done: "Requested", object: "an override"},
	"graph_query":      {doing: "Querying the code graph:", done: "Queried the code graph:", subject: "query"},
	"graph_context":    {doing: "Reading the code graph for", done: "Read the code graph for", list: "files", noun: "file"},
}

// listNames are the fields that name one item of a list argument, in the order
// they are looked for.
var listNames = []string{"label", "role", "name", "type_name", "question", "title", "task_id", "path"}

// toolPrefix is what the tool catalogue puts before every name of its own.
const toolPrefix = "devcouncil_"

// actFor finds how a tool reads, and the subject it has before its arguments
// are looked at: a git tool's subject is the git command it stands for.
func actFor(tool string) (act toolAct, fixed string, ok bool) {
	name := strings.TrimPrefix(tool, toolPrefix)
	if sub, isGit := strings.CutPrefix(name, "git_"); isGit && sub != "" {
		return toolAct{doing: "Running", done: "Ran"}, "git " + strings.ReplaceAll(sub, "_", " "), true
	}
	if strings.HasPrefix(name, "/") {
		// A quick action the operator ran from the composer.
		return toolAct{doing: "Running", done: "Ran"}, name, true
	}
	if act, ok = toolActs[name]; ok {
		return act, "", true
	}
	// A tool with no entry of its own — one an MCP server brought, or one added
	// since — is said to be used, by its own name.
	return toolAct{doing: "Using", done: "Used"}, "", false
}

// callArgs is a call's arguments as fields, or nil when they are not an object.
//
// Arguments are model-authored, so they are read from parsed JSON rather than
// echoed. Echoing the raw string would put whatever the model wrote — newlines
// and escape sequences included — straight into the frame.
func callArgs(ev ui.Event) map[string]any {
	if len(ev.Arguments) == 0 {
		return nil
	}
	var fields map[string]any
	if json.Unmarshal(ev.Arguments, &fields) != nil {
		return nil
	}
	return fields
}

// callSubject is how a call reads: the verb, what it is about, and what
// narrows that. known is false for a tool with no entry in toolActs, whose
// subject is the tool's own name and whose arguments the caller lists.
func callSubject(ev ui.Event, done bool) (verb, subject, where string, fields map[string]any, known bool) {
	act, fixed, known := actFor(ev.Tool)
	verb = act.doing
	if done {
		verb = act.done
	}
	fields = callArgs(ev)
	str := func(key string) string {
		s, _ := fields[key].(string)
		return strings.TrimSpace(oneLine(s))
	}
	switch {
	case !known:
		return verb, ui.OneLine(ev.Tool), "", fields, false
	case fixed != "":
		subject = fixed
	case act.list != "":
		items, _ := fields[act.list].([]any)
		noun := act.noun
		if len(items) != 1 {
			noun += "s"
		}
		verb += " " + groupDigits(len(items)) + " " + noun
		subject = listed(items)
	case ev.Detail != "":
		subject = ev.Detail
	case act.subject != "" && str(act.subject) != "":
		subject = str(act.subject)
	case act.object != "":
		subject = act.object
	default:
		for _, k := range subjectKeys {
			if subject = str(k); subject != "" {
				break
			}
		}
	}
	if subject == "" {
		// A verb with nothing after it says less than the tool's name does.
		verb = "Using"
		if done {
			verb = "Used"
		}
		return verb, ui.OneLine(ev.Tool), "", fields, false
	}
	if act.where != "" {
		where = str(act.where)
	}
	return verb, subject, where, fields, true
}

// listed names the first few items of a list argument and counts the rest.
func listed(items []any) string {
	const show = 4
	var names []string
	for _, item := range items {
		name := ""
		switch v := item.(type) {
		case string:
			name = v
		case map[string]any:
			for _, k := range listNames {
				if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
					name = s
					break
				}
			}
		}
		if name = strings.TrimSpace(oneLine(name)); name == "" {
			continue
		}
		if len(names) == show {
			names = append(names, "+"+groupDigits(len(items)-show)+" more")
			break
		}
		names = append(names, render.TruncateWidth(name, 32, "…"))
	}
	return strings.Join(names, ", ")
}

// callState is how far a call has got, which is how its row is worded.
type callState int

const (
	// callRunning is a call with no result yet: "Reading a.go".
	callRunning callState = iota
	// callDone is one that worked: "Read a.go", and for an edit, its size.
	callDone
	// callFailed is one that came back an error: "Editing a.go · failed". It
	// stays in the present, because "Edited a.go" would say a change was made
	// that was not, and carries no size for the same reason. A command is the
	// exception: one that exits non-zero did run, so it is "Ran go test".
	// The row it is drawn on says that it failed (failedCall).
	callFailed
)

// writes reports whether a tool changes a file: the calls whose subject is
// drawn in the one colour the transcript keeps for that.
func writes(tool string) bool {
	switch strings.TrimPrefix(tool, toolPrefix) {
	case "patch_file", "write_file", "delete_file":
		return true
	}
	return false
}

// callLine is a tool call's row: what was done, to what.
//
// The row is a step quieter than the reply it led to: the work of a turn is a
// log to look back through, and in the text colour every row of it stood level
// with the answer. One thing in it is coloured, and that is a file the turn
// changed, so the edits are what a glance down the log finds.
func callLine(ev ui.Event, th Theme, state callState) render.Line {
	sized := state == callDone
	done := sized
	if state == callFailed {
		act, _, _ := actFor(ev.Tool)
		done = act.doing == "Running"
	}
	verb, subject, where, fields, known := callSubject(ev, done)
	// On a terminal of sixteen colours the secondary text colour is the one
	// dark grey, which is a label's colour and not a row's.
	quiet, quieter := th.Muted(), th.Subtle()
	if th.reducedPalette() {
		quiet, quieter = th.Base(), th.Muted()
	}
	what := quiet
	if writes(ev.Tool) {
		what = th.Status(StatusInfo)
	}
	line := render.Styled(verb, quiet)
	if subject != "" {
		line = line.Append(" "+render.TruncateWidth(subject, 96, "…"), what)
	}
	if !known {
		// Its arguments, since nothing here knows which of them matters.
		if detail := toolDetail(ev); detail != "" {
			line = line.Append("  "+detail, quieter)
		}
		return line
	}
	if where != "" {
		line = line.Append(" in "+render.TruncateWidth(where, 48, "…"), quiet)
	}
	if _, fixed, _ := actFor(ev.Tool); fixed != "" {
		// The tool's name gave the subject, so what it was called with is still
		// to be said: the message of a commit, the ref of a show.
		if detail := toolDetail(ev); detail != "" {
			line = line.Append("  "+detail, quieter)
		}
	}
	if !sized {
		return line
	}
	switch strings.TrimPrefix(ev.Tool, toolPrefix) {
	case "patch_file":
		if added, removed, ok := editSize(fields); ok {
			if added > 0 {
				line = line.Append("  +"+groupDigits(added), render.Style{Fg: th.Success, Bg: th.Bg})
			}
			if removed > 0 {
				line = line.Append("  -"+groupDigits(removed), render.Style{Fg: th.Danger, Bg: th.Bg})
			}
		}
	case "write_file":
		if content, _ := fields["content"].(string); content != "" {
			n := strings.Count(strings.TrimRight(content, "\n"), "\n") + 1
			line = line.Append("  +"+groupDigits(n), render.Style{Fg: th.Success, Bg: th.Bg})
		}
	}
	return line
}

// editSize is how many lines a patch added and removed, from the text it
// replaced and the text it put there.
//
// It is a size for a call that worked: every way the patch tool has of writing
// nothing is an error (devcouncil patchFile), and the caller sizes no failed
// call. A patch allowed to replace its target wherever it occurs has no size
// here, because how many places that was is not in its arguments.
//
// The lines the two texts share at their start and at their end are not
// counted: an edit that quotes three lines of context to change one changed
// one. What is left is exact for a change in one place, and an upper bound for
// one that leaves lines untouched in its middle.
func editSize(fields map[string]any) (added, removed int, ok bool) {
	before, hasBefore := fields["target_content"].(string)
	after, hasAfter := fields["replacement_content"].(string)
	if many, _ := fields["allow_multiple"].(bool); many || !hasBefore || !hasAfter {
		return 0, 0, false
	}
	split := func(s string) []string {
		if s = strings.TrimRight(s, "\n"); s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	a, b := split(before), split(after)
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	return len(b), len(a), true
}

// failedCall is a call's row with what went wrong after it, in the failure
// colour: said is the one line the call printed when that fits the row, and
// otherwise the word "failed". The call gives way to it, not the other way
// round: a long command is cut, and that it failed is still said.
//
// The word is "failed" for a call the gate refused as well. The transcript is
// told that a call came back an error and, separately, what the gate decided;
// it is not told which of two errors a refusal was, and the decision's own row,
// under the call, is where that is said.
func failedCall(call render.Line, th Theme, body int, said string) render.Line {
	lead := "  " + th.Glyphs().Dot + " "
	if said == "" || call.Width()+render.StringWidth(lead+said) > body {
		said = "failed"
	}
	note := lead + said
	room := body - render.StringWidth(note)
	if room < 8 {
		return call.Truncate(body)
	}
	return call.Truncate(room).Append(note, render.Style{Fg: th.Danger, Bg: th.Bg})
}

// confirms reports whether what a call returned says only that it was done.
//
// The file tools answer a write that worked with one line, "patched <path>
// (<n> bytes)", which the row has already said as "Edited <path>". Anything
// else one of them says — a second line, or a first that does not open with the
// tool's own word for having done it — is not this, and is shown.
func confirms(call ui.Event, text string) bool {
	var said string
	switch strings.TrimPrefix(call.Tool, toolPrefix) {
	case "patch_file":
		said = "patched "
	case "write_file":
		said = "wrote "
	case "delete_file":
		said = "deleted "
	default:
		return false
	}
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, said) && !strings.Contains(text, "\n")
}

// doingPhrase is a running call as the working row words it: "Reading a.go",
// "Starting 3 subagents", or "Using <tool>" for a tool with no entry of its own.
func doingPhrase(ev ui.Event) string {
	verb, subject, _, _, _ := callSubject(ev, false)
	if subject == "" {
		return verb
	}
	return verb + " " + render.TruncateWidth(ui.OneLine(subject), 60, "…")
}
