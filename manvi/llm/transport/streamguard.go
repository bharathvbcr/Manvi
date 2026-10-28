package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TruncatedStream is the failure a decoder reports when a stream ended without
// the terminal event its provider's wire defines.
//
// A clean EOF is not evidence that a response finished: connection-delimited
// bodies, a proxy that closes the stream properly after its upstream failed,
// and an HTTP/2 END_STREAM after an upstream fault all end a half-written
// answer the same way a finished one ends. Settled as a success, that answer
// runs its half-received tool calls and records no usage. Every decoder owes
// the same rule — no terminal event, no response — and owes it through this one
// constructor, so the failure is typed the same way everywhere: retryable (a
// fresh attempt or another provider can succeed) and marked as arriving inside
// an accepted response.
//
// missing names the terminal signal that never came, for the operator. Whatever
// usage the stream had reported is not carried here; the stream's
// UsageSoFar answers that for the budget ledger, which settles a truncated
// attempt at that usage or at its worst-case reservation, never at zero.
func TruncatedStream(provider, missing string) *Error {
	return StreamFailure(provider,
		fmt.Sprintf("the stream ended before %s; the response is incomplete and was not used", missing),
		0, true)
}

// FirstTokenBudget is the longest a stream whose idle limit is idle may wait
// for its first output: the larger of DefaultFirstTokenTimeout and idle. It is
// the rule NewSSEWithStall applies, exported so a read that happens before the
// SSE reader exists (PreflightStream) is bounded by the same number. A
// non-positive idle means the operator disabled the watchdog, and the read is
// then unbounded as well.
func FirstTokenBudget(idle time.Duration) time.Duration {
	if idle <= 0 {
		return 0
	}
	return max(DefaultFirstTokenTimeout, idle)
}

// preflightLimit bounds how much of a stream is read looking for an opening
// error frame. The frames that precede one are small and few, so this is
// generous by an order of magnitude and exists only so a server that never
// sends a content frame cannot make the inspection unbounded.
const preflightLimit = 64 << 10

// preflightFrames bounds the same thing by count, for a server that sends many
// tiny preamble frames rather than one large one.
const preflightFrames = 16

// FrameJudge classifies one SSE frame read during PreflightStream.
//
// A non-nil failure rejects the response. Otherwise preamble says whether the
// frame carries no model output, in which case inspection continues; a frame
// that is not preamble means generation has begun, and inspection ends because
// nothing after that point is safe to retry from.
type FrameJudge func(frame string) (preamble bool, failure *Error)

// PreflightStream reads the opening frames of a 2xx streaming response and
// lets judge decide whether the request actually succeeded.
//
// A 200 is not a success on every wire: anthropic documents an overloaded_error
// that arrives as an `event: error` frame inside an otherwise healthy stream,
// and Gemini's interactions endpoint does the same. Judged by status alone,
// each of those is fatal to the turn while the same condition delivered as a
// 529 or 503 would have been retried. This is the shared owner of looking: the
// caller supplies the wire's vocabulary, and the backoff, jitter and attempt
// ceiling stay in PostStream.
//
// What is read is put back: the returned body replays the inspected bytes and
// then continues with the rest of the response, so the decoder sees the stream
// whole. Retrying is only safe because nothing has been handed to the caller's
// consumer yet; the scan ends at the first frame judge does not call preamble.
//
// wait bounds the whole inspection (zero is unbounded). The decoder's stall
// watchdog does not exist yet, so without this a server that sent headers and
// then went mute would hold the call until the caller's context ended. When it
// trips the body is closed and the failure is an ErrStalled, retryable.
func PreflightStream(resp *http.Response, provider string, wait time.Duration, judge FrameJudge) (StreamAccepted, *Error) {
	var tripped bool
	var mu sync.Mutex
	if wait > 0 {
		timer := time.AfterFunc(wait, func() {
			mu.Lock()
			tripped = true
			mu.Unlock()
			_ = resp.Body.Close()
		})
		defer timer.Stop()
	}
	stalled := func() *Error {
		mu.Lock()
		defer mu.Unlock()
		if !tripped {
			return nil
		}
		return &Error{Provider: provider, Err: &ErrStalled{Idle: wait, BeforeFirstToken: true}}
	}

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
			if err != nil && strings.HasSuffix(rest, "\r") {
				// No more bytes are coming, so a trailing CR is a line ending on
				// its own and not the first half of a CRLF. SplitFrame holds it
				// back otherwise, and a stream whose last frame ends in a bare CR
				// would never have that frame judged.
				rest += "\n"
			}
			frame, size, ok := SplitFrame(rest)
			if !ok {
				break
			}
			consumed = min(consumed+size, seen.Len())
			frames++

			preamble, failure := judge(frame)
			if failure != nil {
				return StreamAccepted{}, failure
			}
			if !preamble {
				return replay(&seen, resp.Body), nil
			}
		}

		if err != nil {
			if stall := stalled(); stall != nil {
				return StreamAccepted{}, stall
			}
			// EOF or a read failure: hand back what there is and let the
			// decoder report it. A truncated stream is its problem to describe,
			// not this function's to guess at.
			return replay(&seen, resp.Body), nil
		}
	}
	return replay(&seen, resp.Body), nil
}

// lineEnding reports the width of the line ending at s[i]: 0 when there is none,
// 2 for CRLF, 1 for LF or a bare CR, and -1 for a CR that is the last byte
// seen, which may yet turn out to open a CRLF.
func lineEnding(s string, i int) int {
	switch s[i] {
	case '\n':
		return 1
	case '\r':
		switch {
		case i+1 == len(s):
			return -1
		case s[i+1] == '\n':
			return 2
		}
		return 1
	}
	return 0
}

// SplitFrame returns the first whole frame in rest and how many bytes it
// occupied, terminator included.
//
// A frame ends at a blank line, and every line ending the event-stream grammar
// allows — CRLF, LF, a bare CR, in any mix — counts. A scanner that split on
// "\n\n" alone never saw a frame end in a CRLF-framed stream, and the retry
// policy was skipped on nothing but a line ending. SSE.Next accepts all three,
// and two parsers disagreeing about where a frame ends is a defect whether or
// not a live endpoint exercises it. A trailing CR is undecided, so the frame is
// not reported until the byte after it has arrived.
func SplitFrame(rest string) (frame string, size int, ok bool) {
	for i := 0; i < len(rest); {
		first := lineEnding(rest, i)
		switch {
		case first < 0:
			return "", 0, false
		case first == 0:
			i++
			continue
		}
		next := i + first
		if next == len(rest) {
			return "", 0, false
		}
		second := lineEnding(rest, next)
		switch {
		case second < 0:
			return "", 0, false
		case second > 0:
			return rest[:i], next + second, true
		}
		i = next
	}
	return "", 0, false
}

// FrameFields reads the event name and the joined data lines out of one raw
// SSE frame, as SSE.Next would: comments are ignored, a leading byte order mark
// is dropped, and CRLF, LF and bare-CR endings are all line endings.
func FrameFields(frame string) (name, data string) {
	frame = strings.TrimPrefix(frame, utf8BOM)
	if strings.Contains(frame, "\r") {
		frame = strings.ReplaceAll(strings.ReplaceAll(frame, "\r\n", "\n"), "\r", "\n")
	}
	var parts []string
	for _, line := range strings.Split(frame, "\n") {
		switch {
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			parts = append(parts, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return name, strings.Join(parts, "\n")
}

// replay returns a body that yields the inspected bytes and then the rest of
// the response, so nothing is lost to having looked.
func replay(seen *bytes.Buffer, rest io.ReadCloser) StreamAccepted {
	return StreamAccepted{
		Body: struct {
			io.Reader
			io.Closer
		}{
			Reader: io.MultiReader(bytes.NewReader(seen.Bytes()), rest),
			Closer: rest,
		},
	}
}
