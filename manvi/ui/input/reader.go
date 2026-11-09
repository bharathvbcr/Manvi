package input

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"time"
)

// DefaultEscapeDelay is how long a lone escape byte waits for a sequence to
// finish arriving before it is delivered as the Escape key.
//
// The value is a compromise between two failure modes. Too short, and an arrow
// key over a slow ssh link is split into an Escape plus a stray bracket, which
// types "[A" into the prompt. Too long, and pressing Escape to cancel a running
// turn feels unresponsive. Fifty milliseconds is comfortably longer than the
// inter-byte gap of a sequence sent as one write, and short enough to read as
// instant.
const DefaultEscapeDelay = 50 * time.Millisecond

// DefaultPasteDeadline is how long an open bracketed paste may wait for its end
// marker, measured from the moment the paste opened.
//
// An open paste is not a lone escape: its end marker is expected, so the
// escape grace above would turn any stall in the stream (an ssh hiccup, a
// multiplexer flushing late) into a premature end. Seconds cover those stalls.
// It is not much longer because the deadline is also what ends a paste whose
// marker was lost, and until it fires nothing the user types is delivered.
//
// It is deliberately not extended by arriving bytes. Memory is bounded by
// DefaultMaxPasteBytes, not by time, so there is nothing for an extension to
// protect, and a trickle of bytes must not be able to hold the deadline off
// forever. When it fires the paste is delivered and paste mode ends: what
// arrives next is keys. A genuine paste that was merely slow loses nothing but
// its tail's paste framing, which is the cost of never letting a lost end
// marker hold the keyboard. Ctrl+C ends a paste at once, at any point.
//
// Decision (ft-d01ecfde, 2026-11-09): keep this as it is. Two consequences are
// accepted knowingly. A paste that takes longer than the deadline has its tail
// decoded as keystrokes, so each carriage return in that tail is an Enter. And
// a literal 0x03 inside a paste ends it, as Ctrl+C does. Extending the deadline
// while bytes arrive would trade these for a keyboard that a never-ending
// stream could hold, which is the worse failure; no behaviour change.
const DefaultPasteDeadline = 5 * time.Second

// DefaultPasteIdle is how long input must stay silent before a paste that was
// delivered at the byte limit stops being continued. A stream that is still
// producing a paste after DefaultMaxPasteBytes is live, so its next read is
// well inside this; a second of silence means no end marker is coming.
const DefaultPasteIdle = time.Second

// timerSlot is a timer and the channel to select on, nil while unarmed. Run
// owns three of them; an unarmed slot blocks forever in a select.
type timerSlot struct {
	t *time.Timer
	C <-chan time.Time
}

// armIfIdle starts the timer unless one is already running, so the time it
// measures runs from the first call, not the latest.
func (s *timerSlot) armIfIdle(d time.Duration) {
	if s.t == nil {
		s.t = time.NewTimer(d)
		s.C = s.t.C
	}
}

func (s *timerSlot) stop() {
	if s.t != nil {
		s.t.Stop()
		s.t = nil
		s.C = nil
	}
}

// Reader turns a byte stream into a channel of events.
type Reader struct {
	src           io.Reader
	events        chan Event
	done          chan struct{}
	closeOnce     sync.Once
	escapeDelay   time.Duration
	pasteDeadline time.Duration
	pasteIdle     time.Duration
	pasteLimit    int

	// peakPending is the largest the undecoded buffer grew. Run writes it and
	// the events channel closing publishes it, so a test may read it after the
	// channel is closed; it exists to assert that an open paste is bounded.
	peakPending int
}

// NewReader builds a reader over the terminal's input.
func NewReader(src io.Reader) *Reader {
	return &Reader{
		src:         src,
		events:      make(chan Event, 64),
		done:        make(chan struct{}),
		escapeDelay: DefaultEscapeDelay,

		pasteDeadline: DefaultPasteDeadline,
		pasteIdle:     DefaultPasteIdle,
		pasteLimit:    DefaultMaxPasteBytes,
	}
}

// SetEscapeDelay overrides the escape disambiguation window.
func (r *Reader) SetEscapeDelay(d time.Duration) { r.escapeDelay = d }

// SetPasteDeadline overrides how long an open paste waits for its end marker.
// Like SetEscapeDelay it must be called before Run: Run reads the value without
// synchronisation, so changing it while the reader runs is a data race.
func (r *Reader) SetPasteDeadline(d time.Duration) { r.pasteDeadline = d }

// SetPasteIdle overrides how long input may stay silent before a paste
// delivered at the byte limit stops being continued. Call it before Run.
func (r *Reader) SetPasteIdle(d time.Duration) { r.pasteIdle = d }

// SetMaxPasteBytes overrides how many bytes of one open paste are held before
// they are delivered. It must be called before Run.
func (r *Reader) SetMaxPasteBytes(n int) { r.pasteLimit = n }

// Events is the channel decoded events arrive on. It is closed when the input
// stream ends.
func (r *Reader) Events() <-chan Event { return r.events }

// Close stops delivery.
//
// The reading goroutine is parked in a blocking Read on the tty and cannot be
// interrupted from here; it exits when the descriptor closes or the process
// does. That is deliberate rather than overlooked — the alternatives are a
// read deadline, which is not portable across every tty, or a polling loop that
// burns a core to be interruptible. Nothing is delivered after Close, and the
// goroutine holds no resource beyond its stack.
func (r *Reader) Close() {
	r.closeOnce.Do(func() { close(r.done) })
}

// Inject delivers an event as though the terminal had sent it.
//
// Resizes arrive as signals rather than bytes, and the application should not
// have to select over two sources to find out its window changed. It returns
// false if the reader is closed or its buffer is full, which the caller may
// ignore for anything idempotent — a dropped resize is corrected by the next one.
func (r *Reader) Inject(e Event) bool {
	select {
	case <-r.done:
		return false
	default:
	}
	select {
	case r.events <- e:
		return true
	case <-r.done:
		return false
	default:
		return false
	}
}

// Run pumps the stream until it ends or Close is called. It blocks, and is
// normally started on its own goroutine.
func (r *Reader) Run() {
	raw := make(chan []byte, 8)
	readErr := make(chan error, 1)

	go func() {
		// A buffer large enough that an ordinary paste or a burst of mouse
		// motion arrives in one read. Undersizing it does not lose data, but it
		// does split sequences across reads more often, which is the path the
		// escape timeout has to arbitrate.
		buf := make([]byte, 8192)
		for {
			n, err := r.src.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				select {
				case raw <- chunk:
				case <-r.done:
					return
				}
			}
			if err != nil {
				select {
				case readErr <- err:
				case <-r.done:
				}
				return
			}
		}
	}()

	defer close(r.events)

	var pending []byte
	// scan is how far a paste waiting for its end has been searched; see
	// pasteScan. It is reset whenever bytes are consumed.
	scan := pasteScan{limit: r.pasteLimit}
	next := func(flush bool) (Event, int) {
		ev, n := decodeAt(pending, flush, &scan)
		if n > 0 {
			scan.searched = 0
		}
		return ev, n
	}
	// escape is the lone-escape grace. paste is the deadline of an open paste,
	// armed once when it opens and not restarted by the chunks that follow. idle
	// ends a paste continued past the byte limit when input stops.
	var escape, paste, idle timerSlot
	defer escape.stop()
	defer paste.stop()
	defer idle.stop()

	for {
		// Drain everything the buffer can decide on its own. Consuming bytes
		// ends whatever the paste deadline was timing: a paste that just closed
		// must not lend its deadline to one that opens right behind it.
		for len(pending) > 0 {
			ev, n := next(false)
			if n == 0 {
				break
			}
			paste.stop()
			pending = pending[n:]
			if ev != nil && !r.emit(ev) {
				return
			}
		}

		// What is left after draining is undecidable, and three kinds of it
		// wait differently. An open paste, or a buffer that is the start of a
		// paste marker beyond a bare "ESC [", waits for its end until the paste
		// deadline. The rest of a paste continued past the byte limit is paste
		// text and is held for the next read, with the idle timer as its bound.
		// Anything else is a possible escape sequence and gets the short grace.
		pasteHeld := len(pending) > 0 && !scan.continued &&
			(bytes.HasPrefix(pending, pasteStartBytes) ||
				(len(pending) > 2 && bytes.HasPrefix(pasteStartBytes, pending)))
		switch {
		case len(pending) == 0:
			escape.stop()
			paste.stop()
		case pasteHeld:
			escape.stop()
			paste.armIfIdle(r.pasteDeadline)
		case scan.continued:
			escape.stop()
			paste.stop()
		default:
			paste.stop()
			escape.armIfIdle(r.escapeDelay)
		}
		if scan.continued {
			idle.armIfIdle(r.pasteIdle)
		} else {
			idle.stop()
		}

		select {
		case chunk := <-raw:
			escape.stop()
			idle.stop()
			pending = append(pending, chunk...)
			r.peakPending = max(r.peakPending, len(pending))

		case <-paste.C:
			paste.stop()
			// The end marker is not coming in time. Deliver what is held and
			// leave paste mode: what follows is keys.
			ev, n := next(true)
			pending = pending[n:]
			if ev != nil && !r.emit(ev) {
				return
			}

		case <-idle.C:
			idle.stop()
			// A paste continued past the byte limit went quiet. End it, and
			// deliver the few bytes held back as a possible marker or character.
			if len(pending) > 0 {
				ev, n := next(true)
				pending = pending[n:]
				if ev != nil && !r.emit(ev) {
					return
				}
			}
			scan.continued = false

		case <-escape.C:
			escape.stop()
			// Nothing more is coming. Force a decision on what is held.
			for len(pending) > 0 {
				ev, n := next(true)
				if n == 0 {
					// decode must always consume under flush; refusing to would
					// spin this loop forever. Drop a byte rather than hang.
					pending = pending[1:]
					continue
				}
				pending = pending[n:]
				if ev != nil && !r.emit(ev) {
					return
				}
			}

		case err := <-readErr:
			// Drain what the reader already handed over before deciding the
			// stream is finished.
			//
			// raw and readErr are separate buffered channels, and the reading
			// goroutine writes the last chunk and then the error with nothing
			// between them — so by the time this select runs, both are ready.
			// select chooses uniformly among ready cases, so roughly half the
			// time the error was taken while the final chunk was still sitting
			// in raw, and those bytes were dropped: the loop below found
			// `pending` empty, emitted EOF and returned. The comment here used
			// to say a final keystroke is not lost to the stream ending, and it
			// was only true when the scheduler happened to agree.
			//
			// On a tty this is the last thing typed before the descriptor
			// closes. It surfaced as a test that decoded "ab\x1b[A" into
			// nothing about one run in six.
			for {
				select {
				case chunk := <-raw:
					pending = append(pending, chunk...)
					continue
				default:
				}
				break
			}
			escape.stop()
			// Whatever is buffered is decoded before the error is reported, so
			// a final keystroke is not lost to the stream ending.
			for len(pending) > 0 {
				ev, n := next(true)
				if n == 0 {
					pending = pending[1:]
					continue
				}
				pending = pending[n:]
				if ev != nil && !r.emit(ev) {
					return
				}
			}
			if !errors.Is(err, io.EOF) {
				r.emit(Error{Err: err})
			} else {
				r.emit(Error{Err: io.EOF})
			}
			return

		case <-r.done:
			return
		}
	}
}

// emit delivers an event, reporting false once the reader is closed.
func (r *Reader) emit(e Event) bool {
	select {
	case r.events <- e:
		return true
	case <-r.done:
		return false
	}
}
