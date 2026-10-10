package gemini

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
)

// preflight inspects the opening of a stream and decides whether the request
// actually succeeded.
//
// A 200 is not a success on this wire. The interactions endpoint opens the
// stream, sends interaction.created and interaction.status_update, and only
// then reports a failure as an `event: error` frame. Measured live on
// 2026-08-19 across a five-scenario benchmark, every one of these arrived that
// way and killed its whole turn:
//
//	"gemini-3.7-flash is currently experiencing high demand, spikes in demand
//	 are usually temporary. Please try again later."
//	"Invalid input received."
//
// The second was proved transient rather than structural by replaying the
// byte-identical request afterwards, which was accepted. Both are exactly what
// the retry policy exists for, and neither reached it.
//
// What is read here is put back: the returned reader replays the inspected
// bytes and then continues with the rest of the response, so the decoder sees
// the stream whole and no frame is consumed by looking at it.
//
// Retrying is only safe because the error arrives before any content. The scan
// stops at the first frame that is not part of the preamble, so a stream that
// has begun producing output is never reported as a retryable failure — that
// would re-issue a request whose first half the caller has already read.
func preflight(resp *http.Response) (transport.StreamAccepted, *transport.Error) {
	return transport.PreflightStream(resp, Name, transport.FirstTokenBudget(transport.DefaultHostedStallTimeout),
		func(frame string) (bool, *transport.Error) {
			if failure := errorInFrame(frame); failure != nil {
				return false, failure
			}
			return isPreamble(frame), nil
		})
}

// isPreamble reports whether a frame is one of the opening frames that carry no
// model output. Anything else means generation has begun.
func isPreamble(frame string) bool {
	for _, line := range strings.Split(frame, "\n") {
		if !strings.HasPrefix(line, "event:") {
			continue
		}
		switch strings.TrimSpace(strings.TrimPrefix(line, "event:")) {
		case EventInteractionCreated, EventInteractionInProgress, EventInteractionStatusUpdate:
			return true
		default:
			return false
		}
	}
	// A frame with no event name is judged by its payload below; treat it as
	// content so inspection stops rather than running on.
	return false
}

// errorInFrame returns the failure an `error` frame carries, or nil.
func errorInFrame(frame string) *transport.Error {
	// event_type is deliberately not decoded. A frame is judged by whether it
	// carries an error object, not by what it calls itself: a failure that
	// arrives under a name this decoder has not seen is still a failure, and
	// matching on the name would let it through.
	var payload struct {
		Error *struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	for _, line := range strings.Split(frame, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == DoneSentinel {
			continue
		}
		if json.Unmarshal([]byte(data), &payload) != nil || payload.Error == nil {
			continue
		}
		// `code` is read alongside `status` because this wire uses the first
		// and the decoder was only reading the second — which is why these
		// failures reported an empty parenthesis where the reason belonged.
		reason := payload.Error.Status
		if reason == "" {
			reason = payload.Error.Code
		}
		message := payload.Error.Message
		if reason != "" {
			message += " (" + reason + ")"
		}
		return transport.StreamFailure(Name, message, 0, retryableMessage(payload.Error.Message, reason))
	}
	return nil
}

// retryableMessage decides whether a failure reported in a frame is worth
// another attempt.
//
// It reads the message because that is where this wire puts the distinction and
// the code does not: an overloaded model and a malformed request both arrive as
// `"code":"invalid_request"`. Matching on text is a poor instrument and it is
// the only one offered; what keeps it honest is the direction it fails in.
// Anything unrecognised is retried, because the alternative — treating an
// unknown transient as permanent — is what discarded whole turns, and a retry
// of a genuinely malformed request costs three more refusals and reports the
// same error.
func retryableMessage(message, reason string) bool {
	lower := strings.ToLower(message)
	for _, permanent := range []string{
		"unknown parameter",
		"missing required parameter",
		"the value is invalid for",
		"unknown value",
		"api key",
		"permission",
		"not found",
		"unsupported",
	} {
		if strings.Contains(lower, permanent) {
			return false
		}
	}
	return true
}
