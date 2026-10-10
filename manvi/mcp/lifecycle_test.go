package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingStdin is a stdin that captures every frame written to it.
type recordingStdin struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recordingStdin) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}
func (r *recordingStdin) Close() error { return nil }
func (r *recordingStdin) frames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := strings.TrimSpace(r.buf.String())
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func newWiredClient() (*Client, *recordingStdin) {
	in := &recordingStdin{}
	return &Client{
		cfg:     ServerConfig{Name: "t", Timeout: time.Second},
		stdin:   in,
		pending: map[int64]chan *Response{},
		doneCh:  make(chan struct{}),
	}, in
}

func TestServerRequestsAreAnswered(t *testing.T) {
	cases := []struct {
		name, frame, wantID string
		wantResult          bool
		wantCode            int
	}{
		{"ping", `{"jsonrpc":"2.0","id":7,"method":"ping"}`, `7`, true, 0},
		{"roots", `{"jsonrpc":"2.0","id":"r-1","method":"roots/list"}`, `"r-1"`, false, CodeMethodNotFound},
		{"sampling without jsonrpc member", `{"method":"sampling/createMessage","id":1}`, `1`, false, CodeMethodNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, in := newWiredClient()
			c.dispatch([]byte(tc.frame))
			fr := in.frames()
			if len(fr) != 1 {
				t.Fatalf("server request was not answered exactly once: %q", fr)
			}
			var got struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *RPCError       `json:"error"`
			}
			if err := json.Unmarshal([]byte(fr[0]), &got); err != nil {
				t.Fatal(err)
			}
			if string(got.ID) != tc.wantID {
				t.Fatalf("reply id %s, want %s", got.ID, tc.wantID)
			}
			if tc.wantResult && (string(got.Result) != "{}" || got.Error != nil) {
				t.Fatalf("ping must get an empty result, got %s", fr[0])
			}
			if !tc.wantResult && (got.Error == nil || got.Error.Code != tc.wantCode) {
				t.Fatalf("want error %d, got %s", tc.wantCode, fr[0])
			}
		})
	}
}

func TestServerNotificationsGetNoReply(t *testing.T) {
	c, in := newWiredClient()
	c.dispatch([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`))
	c.dispatch([]byte(`{"jsonrpc":"2.0","id":null,"method":"ping"}`))
	if fr := in.frames(); len(fr) != 0 {
		t.Fatalf("notification was answered: %q", fr)
	}
}

func TestInitializeDoesNotAdvertiseRoots(t *testing.T) {
	c, in := newWiredClient()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = c.Initialize(ctx)
	for _, f := range in.frames() {
		if strings.Contains(f, `"roots"`) {
			t.Fatalf("initialize advertises roots though roots/list is not served: %s", f)
		}
	}
}

func TestCancelledCallSendsNotificationsCancelled(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			c, in := newWiredClient()
			ctx := context.Background()
			if mode == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				time.AfterFunc(30*time.Millisecond, cancel)
			} else {
				c.cfg.Timeout = 30 * time.Millisecond
			}
			err := c.Call(ctx, "tools/call", map[string]any{"name": "x"}, nil)
			if err == nil {
				t.Fatal("call with no reply succeeded")
			}
			fr := in.frames()
			if len(fr) != 2 {
				t.Fatalf("want request then cancellation, got %q", fr)
			}
			var n struct {
				Method string `json:"method"`
				Params struct {
					RequestID int64 `json:"requestId"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(fr[1]), &n) != nil || n.Method != "notifications/cancelled" || n.Params.RequestID != 1 {
				t.Fatalf("second frame is not a cancellation of request 1: %s", fr[1])
			}
			// A late reply is recorded and not delivered anywhere.
			c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			if len(c.Diagnostics()) == 0 || !strings.Contains(strings.Join(c.Diagnostics(), "\n"), "no longer pending") {
				t.Fatalf("late reply was not recorded: %v", c.Diagnostics())
			}
		})
	}
}

func TestInitializeIsNeverCancelled(t *testing.T) {
	c, in := newWiredClient()
	c.cfg.Timeout = 20 * time.Millisecond
	_, _ = c.Initialize(context.Background())
	for _, f := range in.frames() {
		if strings.Contains(f, "notifications/cancelled") {
			t.Fatalf("initialize was cancelled: %s", f)
		}
	}
}
