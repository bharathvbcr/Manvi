package anthropic

import (
	"encoding/json"
	"net/http"

	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// preflight judges the opening of a 200 streaming response.
//
// The streaming documentation says the API "may occasionally send errors in
// the event stream. For example, during periods of high usage, you may receive
// an overloaded_error, which would normally correspond to an HTTP 529 in a
// non-streaming context", and shows it as `event: error` with
// `{"type":"error","error":{"type":"overloaded_error",...}}`. A 529 is retried
// by the transport; the same condition in this form was a plain, permanent
// error. It is retried here for the same reason, and only while nothing but
// message_start and ping has arrived — the first content_block_* event ends the
// inspection, so an error after content has been emitted is never retried.
func preflight(resp *http.Response) (transport.StreamAccepted, *transport.Error) {
	return transport.PreflightStream(resp, Name, transport.FirstTokenBudget(transport.DefaultHostedStallTimeout), judgeFrame)
}

// judgeFrame classifies one opening frame.
func judgeFrame(frame string) (preamble bool, failure *transport.Error) {
	name, data := transport.FrameFields(frame)
	if name == "" && data == "" {
		return true, nil // a comment-only keep-alive
	}
	var ev struct {
		Type  string `json:"type"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// An undecodable frame is not judged here: the decoder reports it, and
	// inspection stops because it cannot say whether content has begun.
	if json.Unmarshal([]byte(data), &ev) != nil {
		return false, nil //nolint:nilerr // the decoder reports it; see above
	}
	kind := ev.Type
	if kind == "" {
		kind = name
	}
	if kind == EventError || name == EventError || ev.Error != nil {
		message := "unspecified"
		retryable := false
		if ev.Error != nil {
			message = ev.Error.Type + ": " + ev.Error.Message
			retryable = retryableErrorType(ev.Error.Type)
		}
		return false, transport.StreamFailure(Name, "stream error: "+message, 0, retryable)
	}
	return kind == EventMessageStart || kind == EventPing, nil
}

// retryableErrorType reports whether an in-stream error type is a transient
// service condition. overloaded_error is the documented 529 and api_error the
// documented 500, both of which the transport retries when they arrive as
// statuses. Every other type — invalid_request_error, authentication_error,
// permission_error, not_found_error, request_too_large, rate_limit_error — is
// treated as permanent: this is the narrow reading, and an unknown type fails
// closed rather than being retried.
func retryableErrorType(t string) bool { return t == "overloaded_error" || t == "api_error" }
