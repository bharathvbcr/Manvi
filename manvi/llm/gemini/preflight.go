package gemini

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"manvi/llm/transport"
)

// preflightLimit bounds how much of a stream is read looking for an opening
// error frame. The frames that precede one are small and few —
// interaction.created and interaction.status_update, a few hundred bytes — so
// this is generous by an order of magnitude and exists only so a server that
// never sends a content frame cannot make the inspection unbounded.
const preflightLimit = 64 << 10

// preflightFrames bounds the same thing by count, for a server that sends many
// tiny preamble frames rather than one large one.
const preflightFrames = 16

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
	var seen bytes.Buffer
	reader := io.LimitReader(resp.Body, preflightLimit)
	buf := make([]byte, 4096)

	// consumed is how much of seen has already been split into whole frames.
	// Without it each read re-scans the buffer from the start, so the first
	// frame is judged again on every pass and the frame budget is spent before
	// the scan ever reaches the frame that matters.
	consumed := 0
	frames := 0
	for frames < preflightFrames {
		n, err := reader.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
		}

		for frames < preflightFrames {
			rest := seen.String()[consumed:]
			idx := strings.Index(rest, "\n\n")
			if idx < 0 {
				break
			}
			frame := rest[:idx]
			consumed += idx + 2
			frames++

			if failure := errorInFrame(frame); failure != nil {
				return transport.StreamAccepted{}, failure
			}
			if !isPreamble(frame) {
				// Content has started. Nothing after this point is safe to
				// retry from, so inspection ends here.
				return replay(&seen, resp.Body), nil
			}
		}

		if err != nil {
			// EOF or a read failure: hand back what there is and let the
			// decoder report it. A truncated stream is its problem to describe,
			// not this function's to guess at.
			return replay(&seen, resp.Body), nil
		}
	}
	return replay(&seen, resp.Body), nil
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

// replay returns a reader that yields the inspected bytes and then the rest of
// the response, so nothing is lost to having looked.
func replay(seen *bytes.Buffer, rest io.ReadCloser) transport.StreamAccepted {
	return transport.StreamAccepted{
		Body: struct {
			io.Reader
			io.Closer
		}{
			Reader: io.MultiReader(bytes.NewReader(seen.Bytes()), rest),
			Closer: rest,
		},
	}
}
