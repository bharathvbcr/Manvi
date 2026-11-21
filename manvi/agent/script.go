package agent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	starjson "go.starlark.net/lib/json"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The script: lookups that chain, in one step.
//
// A batch cannot use one call's answer in the next, so a lookup that depends on
// the one before it costs a step, and every step re-sends the whole
// conversation. devcouncil_script runs a short Starlark program whose only
// functions are the registry's tools, so the second call's arguments can come
// from the first call's answer.
//
// It is hermetic by construction rather than by filtering. The interpreter's
// universe (go.starlark.net/starlark.Universe) holds only pure builtins: no
// open, no network, no clock, no random. The script's predeclared names are
// that universe, the registry's tools and the JSON codec, and nothing else.
// load() has nowhere to go: Thread.Load is left nil, so a load statement fails
// with "load not implemented by this application".
//
// Every call a script makes goes through the loop's own admission (admitInner,
// shared with the batch) and accounting (account, recordPolicy), under a derived
// ID <call id>#<n>, run one at a time. Like a batch's, those calls are never
// tool_use blocks, and never session.ToolCall or session.ToolResult.
//
// A read made inside a script is never a read in full. read_file under a View
// hands back its mark as Result.OnShown instead of acting on it, and a script
// never calls OnShown: whatever the script printed, the model saw a program's
// output, not the file, and the overwrite guard must not take its word for it.

const (
	// scriptMaxSteps bounds the interpreter: Thread.SetMaxExecutionSteps.
	scriptMaxSteps = 1_000_000
	// maxScriptOutput bounds what print() contributes to the result.
	maxScriptOutput = 128 << 10
	// maxScriptError bounds an error's backtrace in the result.
	maxScriptError = 2 << 10
)

// scriptWallTime bounds a script's wall time, tool calls included. A variable
// only so a test can shorten it.
var scriptWallTime = 30 * time.Second

// scriptOptions is the dialect: the Starlark spec's optional features a
// short chaining script needs (for/if at top level, while, set, rebinding a
// name). Recursion stays disabled, which is the default.
var scriptOptions = &syntax.FileOptions{Set: true, While: true, TopLevelControl: true, GlobalReassign: true}

var scriptIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// scriptRun is one script's state. Everything but breach is touched only by the
// goroutine running the interpreter.
type scriptRun struct {
	l        *Loop
	out      *Outcome
	tally    *stepTally
	id       llm.CallID
	maxCalls int
	calls    int
	output   strings.Builder
	fatal    error

	mu     sync.Mutex
	breach string
}

// setBreach records the first reason the script was ended early.
func (r *scriptRun) setBreach(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.breach == "" {
		r.breach = reason
	}
}

func (r *scriptRun) breached() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.breach
}

func (l *Loop) runScript(ctx context.Context, out *Outcome, tally *stepTally, call llm.ToolCallBlock) tools.Result {
	var args struct {
		Script   string `json:"script"`
		MaxCalls *int   `json:"max_calls"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tools.Errorf("bad arguments to %s: %v", tools.ScriptToolName, err)
	}
	if strings.TrimSpace(args.Script) == "" {
		return tools.Errorf("%s needs a script", tools.ScriptToolName)
	}
	if len(args.Script) > tools.MaxScriptBytes {
		return tools.Errorf("the script is %d bytes; the limit is %d", len(args.Script), tools.MaxScriptBytes)
	}
	maxCalls := tools.DefaultScriptCalls
	if args.MaxCalls != nil {
		maxCalls = *args.MaxCalls
	}
	if maxCalls < 1 || maxCalls > tools.MaxScriptCalls {
		return tools.Errorf("max_calls must be in 1..%d, got %d", tools.MaxScriptCalls, maxCalls)
	}

	ctx, cancel := context.WithTimeout(ctx, scriptWallTime)
	defer cancel()
	run := &scriptRun{l: l, out: out, tally: tally, id: call.ID, maxCalls: maxCalls}

	thread := &starlark.Thread{Name: tools.ScriptToolName}
	thread.Print = run.print
	thread.SetMaxExecutionSteps(scriptMaxSteps)
	thread.OnMaxSteps = func(t *starlark.Thread) {
		reason := fmt.Sprintf("the script ran more than %d interpreter steps", scriptMaxSteps)
		run.setBreach(reason)
		t.Cancel(reason)
	}

	// Cancel the interpreter when the context ends, so a script spinning in
	// pure Starlark stops at the wall-time bound or the caller's cancellation.
	done := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		select {
		case <-ctx.Done():
			reason := ctxReason(ctx)
			run.setBreach(reason)
			thread.Cancel(reason)
		case <-done:
		}
	}()
	_, err := starlark.ExecFileOptions(scriptOptions, thread, "script.star", args.Script, run.predeclared(ctx))
	close(done)
	watcher.Wait()

	return run.result(err)
}

// ctxReason names why the script's context ended.
func ctxReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("the script ran past its %s wall-time bound", scriptWallTime)
	}
	return "the script was cancelled"
}

// predeclared is everything the script can name beyond the universe: the
// registry's tools, each a function, and json. A name that is not a plain
// identifier, or that would shadow a universe name, cannot be written in the
// language and is left out.
func (r *scriptRun) predeclared(ctx context.Context) starlark.StringDict {
	names := starlark.StringDict{"json": starjson.Module}
	for _, schema := range r.l.tools.Schemas() {
		name := schema.Name
		if !scriptIdent.MatchString(name) || starlark.Universe.Has(name) || names.Has(name) {
			continue
		}
		names[name] = r.toolFunc(ctx, name)
	}
	return names
}

// print is the script's print(): captured, bounded, and the end of the script
// when the bound is passed.
func (r *scriptRun) print(thread *starlark.Thread, msg string) {
	if r.breached() != "" {
		return
	}
	room := maxScriptOutput - r.output.Len()
	if len(msg)+1 > room {
		r.output.WriteString(clipUTF8(msg, max(0, room)))
		reason := fmt.Sprintf("the script printed more than %d bytes", maxScriptOutput)
		r.setBreach(reason)
		thread.Cancel(reason)
		return
	}
	r.output.WriteString(msg)
	r.output.WriteString("\n")
}

// toolFunc is the Starlark function for one tool: keyword arguments become the
// tool's JSON arguments. It returns a struct, because Starlark has no way to
// catch an error: text is the answer (cut at the per-call byte bound),
// error is true when the call failed or was refused, refused is true when it was
// not run, truncated is true when text was cut.
func (r *scriptRun) toolFunc(ctx context.Context, name string) *starlark.Builtin {
	return starlark.NewBuiltin(name, func(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) > 0 {
			return nil, fmt.Errorf("%s takes keyword arguments only, for example %s(pattern=\"x\")", name, name)
		}
		if err := ctx.Err(); err != nil {
			reason := ctxReason(ctx)
			r.setBreach(reason)
			return nil, errors.New(reason)
		}
		if r.calls >= r.maxCalls {
			reason := fmt.Sprintf("the script made more than its %d calls", r.maxCalls)
			r.setBreach(reason)
			return nil, errors.New(reason)
		}
		r.calls++
		encoded, err := encodeKwargs(thread, kwargs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		callArgs, refusal := r.l.admitInner("a script", name, jsontext.Value(encoded))
		if refusal != "" {
			return scriptResult(refusal, true, true, false), nil
		}

		id := llm.CallID(fmt.Sprintf("%s#%d", r.id, r.calls))
		view := tools.View{MaxLines: maxBatchLines, MaxBytes: maxBatchCallBytes}
		result := r.l.tools.Run(tools.WithView(ctx, view), tools.Call{ID: id, Name: name, Arguments: []byte(callArgs)})
		r.out.ToolCalls++
		r.l.account(r.out, r.tally, name, result)
		if err := r.l.recordPolicy(r.out, id, name, result); err != nil {
			r.fatal = fmt.Errorf("recording script call %d: %w", r.calls, err)
			reason := r.fatal.Error()
			r.setBreach(reason)
			return nil, errors.New(reason)
		}
		// result.OnShown is deliberately not called: see the comment at the top
		// of this file. A read inside a script is not a read in full.
		if len(result.Content) > 0 {
			return scriptResult("[this answer is not text and was left out of the script; call the tool on its own]", true, false, true), nil
		}
		text := result.Text
		truncated := len(text) > maxBatchCallBytes
		if truncated {
			text = clipUTF8(text, maxBatchCallBytes)
		}
		return scriptResult(text, result.IsError, false, truncated), nil
	})
}

func scriptResult(text string, isErr, refused, truncated bool) starlark.Value {
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"text":      starlark.String(text),
		"error":     starlark.Bool(isErr),
		"refused":   starlark.Bool(refused),
		"truncated": starlark.Bool(truncated),
	})
}

// encodeKwargs is a call's keyword arguments as a JSON object, through the
// interpreter's own json.encode so every Starlark value has one encoding.
func encodeKwargs(thread *starlark.Thread, kwargs []starlark.Tuple) ([]byte, error) {
	dict := starlark.NewDict(len(kwargs))
	for _, kv := range kwargs {
		if err := dict.SetKey(kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	encoded, err := starlark.Call(thread, starjson.Module.Members["encode"], starlark.Tuple{dict}, nil)
	if err != nil {
		return nil, fmt.Errorf("arguments are not JSON: %w", err)
	}
	text, ok := starlark.AsString(encoded)
	if !ok {
		return nil, errors.New("arguments did not encode to text")
	}
	return []byte(text), nil
}

// result is what the model is shown: the script's output, and when the script
// did not run to its end, the reason, after whatever it had produced.
func (r *scriptRun) result(execErr error) tools.Result {
	var text strings.Builder
	fmt.Fprintf(&text, "script: %d call(s) made\n", r.calls)
	text.WriteString(r.output.String())
	if r.output.Len() > 0 && !strings.HasSuffix(r.output.String(), "\n") {
		text.WriteString("\n")
	}
	breach := r.breached()
	switch {
	case breach != "":
		fmt.Fprintf(&text, "[script ended early: %s; the output above is what it produced before that]\n", breach)
	case execErr != nil:
		msg := execErr.Error()
		var eval *starlark.EvalError
		if errors.As(execErr, &eval) {
			msg = eval.Backtrace()
		}
		fmt.Fprintf(&text, "[script error: %s]\n", clipUTF8(msg, maxScriptError))
	}
	return tools.Result{Text: text.String(), IsError: breach != "" || execErr != nil}
}

// clipUTF8 is the first n bytes of s, ended on a rune boundary.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
