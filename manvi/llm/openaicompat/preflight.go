package openaicompat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// preflight judges the opening of a 200 streaming response.
//
// An `error` chunk arriving before any choice delta is the same condition as an
// HTTP error status — the provider accepted the connection and then reported a
// failure — so it is classified and retried like one rather than failing the
// turn. The decision, recorded here because the OpenAI-compatible wire does not
// document in-stream errors the way anthropic does: a failure is retried only
// when its own type or code names a transient service condition (see
// retryableWireError). Anything else, including a bare-string error from a
// local server, is permanent, because retrying a malformed request four times
// reports the same refusal after the backoff.
//
// Inspection ends at the first chunk that carries a delta, a finish_reason or
// usage, so an error after output has begun is never retried: nothing may be
// re-issued once a chunk could have reached the caller.
func preflight(wait time.Duration, name string) func(*http.Response) (transport.StreamAccepted, *transport.Error) {
	return func(resp *http.Response) (transport.StreamAccepted, *transport.Error) {
		return transport.PreflightStream(resp, name, wait, func(frame string) (bool, *transport.Error) {
			return judgeFrame(name, frame)
		})
	}
}

// judgeFrame classifies one opening frame.
func judgeFrame(name, frame string) (preamble bool, failure *transport.Error) {
	_, data := transport.FrameFields(frame)
	data = strings.TrimSpace(data)
	if data == "" {
		return true, nil // a comment-only keep-alive
	}
	var chunk wireChunk
	// Undecodable and [DONE] frames are the decoder's to report; inspection
	// stops because it cannot say whether output has begun.
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return false, nil
	}
	if chunk.Error != nil {
		message := fmt.Sprintf("%s: stream error: %s", name, chunk.Error.Message)
		return false, transport.StreamFailure(name, message, 0, retryableWireError(chunk.Error))
	}
	if chunk.Usage != nil {
		return false, nil
	}
	for _, choice := range chunk.Choices {
		d := choice.Delta
		if d.Content != nil && *d.Content != "" || d.ReasoningContent != nil && *d.ReasoningContent != "" ||
			d.Reasoning != nil && *d.Reasoning != "" || len(d.ToolCalls) > 0 ||
			choice.FinishReason != nil && *choice.FinishReason != "" {
			return false, nil
		}
	}
	// A role-only or empty delta: the opening frame most servers send first.
	return true, nil
}

// retryableWireError reports whether an in-stream error names a transient
// service condition. The vocabulary is OpenAI's documented error `type`/`code`
// values for server-side and capacity faults; everything else is permanent.
func retryableWireError(e *wireError) bool {
	for _, v := range []string{e.Type, fmt.Sprint(e.Code)} {
		switch strings.ToLower(v) {
		case "server_error", "overloaded_error", "overloaded", "service_unavailable",
			"rate_limit_exceeded", "rate_limit_error", "timeout":
			return true
		}
	}
	return false
}
