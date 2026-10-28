package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type trackBody struct {
	ctx    context.Context
	r      io.Reader
	closed *atomic.Int64
}

func (b *trackBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, context.Cause(b.ctx)
	}
	return b.r.Read(p)
}
func (b *trackBody) Close() error { b.closed.Add(1); return nil }

// TestHeaderTimerRaceOnTheRealClock lands the answer at about the limit, from
// many goroutines sharing one Client, so the claim in Client.send is decided by
// the real scheduler rather than by a held clock. Every outcome must be a whole
// one: a readable response or a typed timeout, and every body the transport
// produced closed exactly once.
func TestHeaderTimerRaceOnTheRealClock(t *testing.T) {
	var made, closedN atomic.Int64
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		d := time.Duration(900+int(made.Load()%200)) * time.Microsecond
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return nil, context.Cause(r.Context())
		}
		made.Add(1)
		return &http.Response{StatusCode: 200, Request: r,
			Body: &trackBody{ctx: r.Context(), r: strings.NewReader("data: x\n\n"), closed: &closedN}}, nil
	})
	c := &Client{Provider: "p", BaseURL: "http://x", HTTP: &http.Client{Transport: rt}, ResponseHeaderTimeout: time.Millisecond}
	var wg sync.WaitGroup
	var kept, timedOut, bad atomic.Int64
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				req, _ := http.NewRequest("POST", "http://x/y", nil)
				resp, err := c.send(context.Background(), req)
				var to *ErrHeaderTimeout
				switch {
				case err == nil && resp != nil:
					b, rerr := io.ReadAll(resp.Body)
					if rerr != nil || string(b) != "data: x\n\n" {
						bad.Add(1)
						t.Errorf("kept response unreadable: %q %v", b, rerr)
					}
					resp.Body.Close()
					kept.Add(1)
				case errors.As(err, &to) && resp == nil:
					timedOut.Add(1)
				default:
					bad.Add(1)
					t.Errorf("resp=%v err=%v", resp, err)
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("kept=%d timedOut=%d made=%d closed=%d", kept.Load(), timedOut.Load(), made.Load(), closedN.Load())
	// every response the transport produced is closed exactly once (either by caller on keep, or by send on timeout)
	if made.Load() != closedN.Load() {
		t.Fatalf("body leak: made %d closed %d", made.Load(), closedN.Load())
	}
}
