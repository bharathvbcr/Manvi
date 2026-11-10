package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// Serve tests do not use t.Parallel, so a test may set these package knobs for
// its own duration. The restore is registered before the server starts, so it
// runs after the server has stopped.
func setForTest[T any](t *testing.T, target *T, value T) {
	t.Helper()
	was := *target
	*target = value
	t.Cleanup(func() { *target = was })
}

// socketTestDir is a short private directory: a unix socket address holds about
// a hundred bytes, which t.TempDir on macOS can exceed.
func socketTestDir(t *testing.T) string {
	t.Helper()
	if !peerCredSupported {
		t.Skip("socket-served mode needs peer credentials, which this platform does not provide")
	}
	dir, err := os.MkdirTemp("", "mvs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func readSocketTestFile(t *testing.T, path string) string {
	t.Helper()
	got, err := os.ReadFile(path) // #nosec G304 -- a path under this test's own temp directory
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// socketHarness is a socket server running in a goroutine.
type socketHarness struct {
	t      *testing.T
	srv    *SocketServer
	log    *lockedBuffer
	cancel context.CancelFunc
	exited chan struct{}
	err    error
}

func startSocketServer(t *testing.T, cfg SocketConfig) *socketHarness {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = filepath.Join(socketTestDir(t), "s")
	}
	if cfg.IdleExit == 0 {
		cfg.IdleExit = time.Hour
	}
	log := &lockedBuffer{}
	cfg.Log = log
	srv, err := ListenSocket(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &socketHarness{t: t, srv: srv, log: log, cancel: cancel, exited: make(chan struct{})}
	go func() {
		h.err = srv.Serve(ctx)
		close(h.exited)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.exited:
		case <-time.After(20 * time.Second):
			t.Error("the socket server did not stop")
		}
	})
	return h
}

// stop ends the server the way a signal does and returns what Serve returned.
func (h *socketHarness) stop() error {
	h.t.Helper()
	h.cancel()
	h.await()
	return h.err
}

func (h *socketHarness) await() {
	h.t.Helper()
	select {
	case <-h.exited:
	case <-time.After(20 * time.Second):
		h.t.Fatal("the socket server did not stop")
	}
}

func (h *socketHarness) hasExited() bool {
	select {
	case <-h.exited:
		return true
	default:
		return false
	}
}

func (s *SocketServer) hasController() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctrl != nil
}

func (h *socketHarness) awaitNoController() {
	h.t.Helper()
	waitFor(h.t, "the controller slot to free", func() bool { return !h.srv.hasController() })
}

func (h *socketHarness) dial() *socketClient {
	h.t.Helper()
	return dialSocket(h.t, h.srv.Path())
}

type socketClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	n    int
}

func dialSocket(t *testing.T, path string) *socketClient {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c := &socketClient{t: t, conn: conn, r: bufio.NewReader(conn)}
	t.Cleanup(c.close)
	return c
}

func (c *socketClient) close() { _ = c.conn.Close() }

func (c *socketClient) send(op string, params any) string {
	c.t.Helper()
	c.n++
	id := fmt.Sprintf("c%d", c.n)
	req := Request{ID: id, Op: op}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			c.t.Fatal(err)
		}
		req.Params = raw
	}
	line, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		c.t.Fatalf("write %s: %v", op, err)
	}
	return id
}

// read returns the next line the server wrote, or the error that ended the wait.
func (c *socketClient) read(timeout time.Duration) (Response, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return Response{}, err
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("undecodable line %q: %w", line, err)
	}
	return resp, nil
}

func (c *socketClient) call(op string, params any) Response {
	c.t.Helper()
	id := c.send(op, params)
	resp, err := c.read(10 * time.Second)
	if err != nil {
		c.t.Fatalf("%s: %v", op, err)
	}
	if resp.ID != id {
		c.t.Fatalf("%s was answered as %q, want %q", op, resp.ID, id)
	}
	return resp
}

// expectClosed fails unless the server ends the connection.
func (c *socketClient) expectClosed() {
	c.t.Helper()
	resp, err := c.read(5 * time.Second)
	if err == nil {
		c.t.Fatalf("the server answered a connection it should have closed: %+v", resp)
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		c.t.Fatal("the server did not close the connection")
	}
}

// expectSilence fails if the server writes anything within d.
func (c *socketClient) expectSilence(d time.Duration) {
	c.t.Helper()
	if _, err := c.read(d); err == nil {
		c.t.Fatal("the server wrote a line nobody asked for")
	} else {
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			c.t.Fatalf("the connection ended while waiting for silence: %v", err)
		}
	}
}

func socketResult[T any](t *testing.T, resp Response) T {
	t.Helper()
	if !resp.OK {
		t.Fatalf("the op failed: %+v", resp.Error)
	}
	var out T
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func wantResponseCode(t *testing.T, resp Response, code string) Error {
	t.Helper()
	if resp.OK || resp.Error == nil || resp.Error.Code != code {
		t.Fatalf("got %+v, want a %s error", resp, code)
	}
	return *resp.Error
}

// holdModule registers ops that hold their lane: test.hold until released or
// cancelled, test.stubborn until released whatever its context says.
type holdModule struct {
	entered   chan struct{}
	release   chan struct{}
	cancelled atomic.Bool
}

func newHoldModule() *holdModule {
	return &holdModule{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (m *holdModule) Configure(r *Router) error {
	if err := r.Register("test.hold", func(ctx context.Context, _ json.RawMessage) (any, *Error) {
		m.entered <- struct{}{}
		select {
		case <-m.release:
			return map[string]bool{"released": true}, nil
		case <-ctx.Done():
			m.cancelled.Store(true)
			return nil, &Error{Code: ErrDependency, Message: "cancelled"}
		}
	}); err != nil {
		return err
	}
	return r.Register("test.stubborn", func(context.Context, json.RawMessage) (any, *Error) {
		m.entered <- struct{}{}
		<-m.release
		return map[string]bool{"released": true}, nil
	})
}

func (m *holdModule) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-m.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the op never started")
	}
}

var ownerPattern = regexp.MustCompile(`^manvi-(\d+)-(\d+)-[0-9a-f]+$`)

func TestSocketServesTheHostProtocolToAnAcceptedClient(t *testing.T) {
	h := startSocketServer(t, SocketConfig{Options: hostOpts()})
	c := h.dial()
	hello := socketResult[HelloResult](t, c.call(OpHello, HelloParams{Protocol: ProtocolVersion}))
	if hello.Protocol != ProtocolVersion {
		t.Fatalf("protocol %d", hello.Protocol)
	}
	for _, op := range []string{OpServeStatus, OpServeStopAll, OpServeTakeover, OpPolicyCheckFile} {
		if _, ok := hello.OpLimits[op]; !ok {
			t.Errorf("hello does not advertise %s", op)
		}
	}
	// The ops are limited and laned by the same functions that enforce them.
	if got := hello.OpLimits[OpServeStopAll]; got.Lane != "control" || got.DeadlineMS != opDeadline(OpServeStopAll).Milliseconds() {
		t.Fatalf("serve.stop_all advertised %+v", got)
	}
	if got := hello.OpLimits[OpServeStatus]; got.Lane != "inline" {
		t.Fatalf("serve.status advertised %+v", got)
	}

	status := socketResult[ServeStatusResult](t, c.call(OpServeStatus, nil))
	match := ownerPattern.FindStringSubmatch(status.OwnerID)
	if match == nil {
		t.Fatalf("the owner id %q is not manvi-{pid}-{nanos}-{token}", status.OwnerID)
	}
	if match[1] != fmt.Sprint(os.Getpid()) || status.ProcessID != os.Getpid() {
		t.Fatalf("the owner id names pid %s and the status pid %d, not this process", match[1], status.ProcessID)
	}
	if status.OwnerID != h.srv.OwnerID() || status.Protocol != ProtocolVersion || !status.Controller || status.Jobs == nil || len(status.Jobs) != 0 || status.IdleExitMS != time.Hour.Milliseconds() {
		t.Fatalf("status: %+v", status)
	}
	// The policy plane answers exactly as it does on stdio.
	resp := c.call(OpPolicyCheckFile, FileCheckParams{Path: "/etc/passwd", Root: "/tmp"})
	if !resp.OK {
		t.Fatalf("policy.check.file over the socket: %+v", resp.Error)
	}
}

func TestSocketIsPrivateAndItsDirectoryIsCreatedPrivate(t *testing.T) {
	parent := socketTestDir(t)
	path := filepath.Join(parent, "fresh", "s")
	h := startSocketServer(t, SocketConfig{Path: path, Options: hostOpts()})
	dirInfo, err := os.Lstat(filepath.Dir(h.srv.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("the directory it created is %v", dirInfo.Mode())
	}
	info, err := os.Lstat(h.srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("the socket is %v, want a 0600 socket", info.Mode())
	}
	if owner, ok := fileOwner(info); !ok || owner != uint32(os.Geteuid()) { // #nosec G115 -- a uid is not negative on a platform with peer credentials
		t.Fatalf("the socket is owned by uid %d", owner)
	}
}

func TestSocketRefusesWhatItCouldNotMakeSafe(t *testing.T) {
	cases := map[string]struct {
		prepare func(t *testing.T, dir string) string // returns the socket path
		want    string
		after   func(t *testing.T, dir string)
	}{
		"symlinked socket path": {
			prepare: func(t *testing.T, dir string) string {
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(dir, "s")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				return link
			},
			want: "symbolic link",
			after: func(t *testing.T, dir string) {
				if got := readSocketTestFile(t, filepath.Join(dir, "target")); got != "precious" {
					t.Fatalf("the link's target was touched: %q", got)
				}
				if info, err := os.Lstat(filepath.Join(dir, "s")); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("the link was replaced: %v %v", info, err)
				}
			},
		},
		"symlinked directory": {
			prepare: func(t *testing.T, dir string) string {
				real := filepath.Join(dir, "real")
				if err := os.Mkdir(real, 0o700); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(dir, "link")
				if err := os.Symlink(real, link); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(link, "s")
			},
			want: "symbolic link",
			after: func(t *testing.T, dir string) {
				if entries, err := os.ReadDir(filepath.Join(dir, "real")); err != nil || len(entries) != 0 {
					t.Fatalf("something was made through the link: %v %v", entries, err)
				}
			},
		},
		"directory open to others": {
			prepare: func(t *testing.T, dir string) string {
				open := filepath.Join(dir, "open")
				if err := os.Mkdir(open, 0o755); err != nil { // #nosec G301 -- the case under test is a directory others can enter
					t.Fatal(err)
				}
				if err := os.Chmod(open, 0o755); err != nil { // #nosec G302 -- same: the umask must not hide the case
					t.Fatal(err)
				}
				return filepath.Join(open, "s")
			},
			want: "accessible to other users",
			after: func(t *testing.T, dir string) {
				// Checked, never repaired: the operator's directory is not ours to change.
				if info, err := os.Lstat(filepath.Join(dir, "open")); err != nil || info.Mode().Perm() != 0o755 {
					t.Fatalf("the directory was changed: %v %v", info, err)
				}
			},
		},
		"a regular file at the path": {
			prepare: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "s")
				if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			want: "not a socket",
			after: func(t *testing.T, dir string) {
				if got := readSocketTestFile(t, filepath.Join(dir, "s")); got != "keep me" {
					t.Fatalf("the file was replaced: %q", got)
				}
			},
		},
		"a path that is not absolute": {
			prepare: func(*testing.T, string) string { return "relative/s" },
			want:    "absolute",
		},
		"a path past the address limit": {
			prepare: func(_ *testing.T, dir string) string { return filepath.Join(dir, strings.Repeat("a", 120)) },
			want:    "at most",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := socketTestDir(t)
			path := c.prepare(t, dir)
			_, err := ListenSocket(t.Context(), SocketConfig{Path: path, Options: hostOpts(), IdleExit: time.Hour})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
			if c.after != nil {
				c.after(t, dir)
			}
		})
	}
}

func TestSocketReplacesAStaleSocketButNeverALiveOne(t *testing.T) {
	dir := socketTestDir(t)
	path := filepath.Join(dir, "s")

	// A socket file nothing is listening on is what a killed server leaves.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("the fixture did not leave a socket: %v %v", info, err)
	}
	live := startSocketServer(t, SocketConfig{Path: path, Options: hostOpts()})
	c := live.dial()
	if !c.call(OpHello, nil).OK {
		t.Fatal("the server on the replaced socket does not answer")
	}

	// A second server on the same path is refused, and the first is undisturbed.
	_, err = ListenSocket(t.Context(), SocketConfig{Path: path, Options: hostOpts(), IdleExit: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "already being served") {
		t.Fatalf("a live socket was taken over: %v", err)
	}
	if !c.call(OpHello, nil).OK {
		t.Fatal("the first server stopped answering after the refused second")
	}
}

func TestSocketClosesAConnectionWhosePeerIsNotThisUserWithoutAnswering(t *testing.T) {
	for name, peer := range map[string]func(*net.UnixConn) (uint32, error){
		"another uid":        func(*net.UnixConn) (uint32, error) { return uint32(os.Geteuid()) + 1, nil }, // #nosec G115 -- a uid is not negative on a platform with peer credentials
		"an unreadable peer": func(*net.UnixConn) (uint32, error) { return 0, errors.New("no credentials") },
	} {
		t.Run(name, func(t *testing.T) {
			h := startSocketServer(t, SocketConfig{Options: hostOpts(), peer: peer})
			c := h.dial()
			// The server may close before this is written; either way nothing answers.
			_, _ = c.conn.Write([]byte(`{"id":"1","op":"hello"}` + "\n"))
			c.expectClosed()
			waitFor(t, "the refusal to be logged", func() bool { return strings.Contains(h.log.String(), "refused a connection") })
			if h.srv.hasController() {
				t.Fatal("a refused peer holds the controller slot")
			}
		})
	}
}

// The check is the kernel's record, not a stand-in: the uid it reports for a
// real connection from this process is this process's.
func TestPeerUIDIsTheKernelsRecordForARealConnection(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "s")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan *net.UnixConn, 1)
	go func() {
		conn, err := ln.AcceptUnix()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()
	client, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server := <-accepted
	if server == nil {
		t.Fatal("nothing was accepted")
	}
	t.Cleanup(func() { _ = server.Close() })
	uid, err := peerUID(server)
	if err != nil || uid != uint32(os.Geteuid()) { // #nosec G115 -- a uid is not negative on a platform with peer credentials
		t.Fatalf("peerUID = %d, %v; this process runs as %d", uid, err, os.Geteuid())
	}
}

func TestSocketSecondClientIsRefusedWhileOneControlsAndMayOnlyAskOrTakeOver(t *testing.T) {
	mod := newHoldModule()
	h := startSocketServer(t, SocketConfig{Options: Options{HardRules: true, Modules: []Module{mod}}})
	a := h.dial()
	if !a.call(OpHello, nil).OK {
		t.Fatal("the first client does not control")
	}

	b := h.dial()
	// Everything but status and takeover is turned away, retryably, and never
	// reaches the op: B has no way to write.
	for _, op := range []string{OpHello, OpPolicyCheckFile, OpServeStopAll, "test.hold", "work.runs.managed.stop"} {
		e := wantResponseCode(t, b.call(op, nil), ErrControllerBusy)
		if !e.Retryable || !strings.Contains(e.Message, OpServeTakeover) {
			t.Fatalf("%s: %+v", op, e)
		}
	}
	select {
	case <-mod.entered:
		t.Fatal("a waiting connection reached an op")
	default:
	}
	waiting := socketResult[ServeStatusResult](t, b.call(OpServeStatus, nil))
	if waiting.Controller || waiting.OwnerID != h.srv.OwnerID() {
		t.Fatalf("a waiting connection's status: %+v", waiting)
	}
	// Garbage is answered, not fatal.
	if _, err := b.conn.Write([]byte("not json\n")); err != nil {
		t.Fatal(err)
	}
	if resp, err := b.read(5 * time.Second); err != nil || resp.OK || resp.Error == nil || resp.Error.Code != ErrBadRequest {
		t.Fatalf("garbage on a waiting connection: %+v %v", resp, err)
	}
	// And A is untouched by all of it.
	if !socketResult[ServeStatusResult](t, a.call(OpServeStatus, nil)).Controller {
		t.Fatal("the controller lost control to a waiting connection")
	}

	// A takes over from nobody: it is already the controller.
	own := socketResult[ServeTakeoverResult](t, a.call(OpServeTakeover, nil))
	if !own.Controller || own.TookOver {
		t.Fatalf("takeover by the controller: %+v", own)
	}

	// B replaces A on purpose. A's connection is closed; B controls.
	took := socketResult[ServeTakeoverResult](t, b.call(OpServeTakeover, nil))
	if !took.Controller || !took.TookOver {
		t.Fatalf("takeover: %+v", took)
	}
	a.expectClosed()
	if !socketResult[ServeStatusResult](t, b.call(OpServeStatus, nil)).Controller {
		t.Fatal("the new controller does not control")
	}
	if !b.call(OpHello, nil).OK {
		t.Fatal("the new controller cannot send ops")
	}
	if !strings.Contains(h.log.String(), "took over") {
		t.Fatalf("the takeover was not logged:\n%s", h.log.String())
	}
}

func TestSocketNewClientAfterTheControllerDisconnectedIsServedWithoutATakeover(t *testing.T) {
	h := startSocketServer(t, SocketConfig{Options: hostOpts()})
	a := h.dial()
	if !a.call(OpHello, nil).OK {
		t.Fatal("no first controller")
	}
	a.close()
	h.awaitNoController()
	b := h.dial()
	if !b.call(OpHello, nil).OK {
		t.Fatal("the next client after a disconnect was not served")
	}
	// A takeover that finds the slot free claims it and says nobody was replaced.
	b.close()
	h.awaitNoController()
	c := h.dial()
	took := socketResult[ServeTakeoverResult](t, c.call(OpServeTakeover, nil))
	if !took.Controller || took.TookOver {
		t.Fatalf("%+v", took)
	}
}

// The slot is not free while the previous controller's connection is still being
// served out, so a new client never runs beside a request the old one left in
// flight. Those requests are given the grace and then cancelled.
func TestSocketSlotIsFreeOnlyOnceTheOldControllersRequestsAreOver(t *testing.T) {
	setForTest(t, &disconnectGrace, 200*time.Millisecond)
	mod := newHoldModule()
	h := startSocketServer(t, SocketConfig{Options: Options{Modules: []Module{mod}}})
	a := h.dial()
	a.send("test.hold", nil)
	mod.awaitEntered(t)
	a.close()

	b := h.dial()
	wantResponseCode(t, b.call(OpHello, nil), ErrControllerBusy)
	if mod.cancelled.Load() {
		t.Fatal("the request was cancelled at once, not after the grace")
	}
	waitFor(t, "the abandoned request to be cancelled", mod.cancelled.Load)
	h.awaitNoController()
	c := h.dial()
	if !c.call(OpHello, nil).OK {
		t.Fatal("no client was served after the old controller's requests ended")
	}
}

// Taking over cancels the controller's requests and claims the slot only once
// they have ended: the answer to the takeover is the proof.
func TestSocketTakeoverWaitsForTheOldControllersRequestsToEnd(t *testing.T) {
	mod := newHoldModule()
	h := startSocketServer(t, SocketConfig{Options: Options{Modules: []Module{mod}}})
	a := h.dial()
	a.send("test.hold", nil)
	mod.awaitEntered(t)
	b := h.dial()
	took := socketResult[ServeTakeoverResult](t, b.call(OpServeTakeover, nil))
	if !took.TookOver {
		t.Fatalf("%+v", took)
	}
	if !mod.cancelled.Load() {
		t.Fatal("the new controller was told it controls while the old one's request was still running")
	}
	a.expectClosed()
}

func TestSocketTakeoverIsRefusedRetryablyWhenTheOldControllerWillNotEnd(t *testing.T) {
	setForTest(t, &takeoverWait, 150*time.Millisecond)
	mod := newHoldModule()
	h := startSocketServer(t, SocketConfig{Options: Options{Modules: []Module{mod}}})
	a := h.dial()
	a.send("test.stubborn", nil) // ignores its context
	mod.awaitEntered(t)

	b := h.dial()
	e := wantResponseCode(t, b.call(OpServeTakeover, nil), ErrControllerBusy)
	if !e.Retryable || !strings.Contains(e.Message, "nothing changed hands") {
		t.Fatalf("%+v", e)
	}
	// Fail closed: the old controller has not been replaced, and B still cannot write.
	if !h.srv.hasController() {
		t.Fatal("the slot was freed with the old controller's request still running")
	}
	wantResponseCode(t, b.call(OpHello, nil), ErrControllerBusy)
	a.expectClosed()

	close(mod.release)
	h.awaitNoController()
	took := socketResult[ServeTakeoverResult](t, b.call(OpServeTakeover, nil))
	if !took.Controller || took.TookOver {
		t.Fatalf("the retry after the old request ended: %+v", took)
	}
	if !b.call(OpHello, nil).OK {
		t.Fatal("the controller after the retry cannot send ops")
	}
}

func TestSocketTwoTakeoversAtOnceLeaveExactlyOneController(t *testing.T) {
	h := startSocketServer(t, SocketConfig{Options: hostOpts()})
	a := h.dial()
	if !a.call(OpHello, nil).OK {
		t.Fatal("no first controller")
	}
	b, c := h.dial(), h.dial()
	ids := [2]string{b.send(OpServeTakeover, nil), c.send(OpServeTakeover, nil)}
	var wins int
	for i, client := range []*socketClient{b, c} {
		resp, err := client.read(10 * time.Second)
		if errors.Is(err, io.EOF) {
			// Replaced by the other takeover before its own answer was written:
			// a takeover closes whoever controls, and this one controlled for
			// no time at all. The ends of the run still hold (one winner, one
			// writer); only a hang or a wrong answer is a failure here.
			continue
		}
		if err != nil || resp.ID != ids[i] {
			t.Fatalf("takeover %d: %+v %v", i, resp, err)
		}
		if resp.OK {
			wins++
		} else {
			wantResponseCode(t, resp, ErrControllerBusy)
		}
	}
	// One of them may have been told it controls and then been replaced by the
	// other, which is what a takeover does to whoever controls. What may never
	// happen is two that can write.
	if wins == 0 {
		t.Fatal("neither takeover succeeded")
	}
	working := 0
	for _, client := range []*socketClient{b, c} {
		_, _ = client.conn.Write([]byte(`{"id":"probe","op":"hello"}` + "\n"))
		if resp, err := client.read(3 * time.Second); err == nil && resp.ID == "probe" && resp.OK {
			working++
		}
	}
	if working != 1 {
		t.Fatalf("%d clients can send ops; exactly one may", working)
	}
}

func TestSocketConnectionsAreBounded(t *testing.T) {
	h := startSocketServer(t, SocketConfig{Options: hostOpts()})
	clients := make([]*socketClient, 0, maxSocketConns)
	for range maxSocketConns {
		c := h.dial()
		c.call(OpServeStatus, nil)
		clients = append(clients, c)
	}
	extra := h.dial()
	// Closed before it was heard, so the write itself may already fail.
	_, _ = extra.conn.Write([]byte(`{"id":"1","op":"serve.status"}` + "\n"))
	extra.expectClosed()
	// Freeing one admits the next.
	clients[len(clients)-1].close()
	waitFor(t, "a connection slot to free", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		return len(h.srv.conns) < maxSocketConns
	})
	if !h.dial().call(OpServeStatus, nil).OK {
		t.Fatal("a connection was refused with a slot free")
	}
}

// socketForLifecycle serves a managed-run fixture's runner over a socket.
func socketForLifecycle(t *testing.T, m *managedLifecycle, mutate func(*SocketConfig)) *socketHarness {
	t.Helper()
	cfg := SocketConfig{
		Options: Options{Modules: []Module{WorkbenchModule{Client: m.client, Managed: m.runner}}},
		Managed: m.runner,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return startSocketServer(t, cfg)
}

func (m *managedLifecycle) awaitTurn(n int) {
	m.t.Helper()
	waitFor(m.t, fmt.Sprintf("turn %d to begin", n), func() bool { return m.state().Turn == n })
}

func TestSocketJobRunsOnWhileNoClientIsConnectedAndTheNextClientAdoptsIt(t *testing.T) {
	m := continuable(t, func(s *managedTestSession) { s.leaveOpen = true })
	m.startContinuable(2)
	m.awaitTurn(1)
	h := socketForLifecycle(t, m, nil)

	a := h.dial()
	before := socketResult[ServeStatusResult](t, a.call(OpServeStatus, nil))
	if len(before.Jobs) != 1 {
		t.Fatalf("jobs: %+v", before.Jobs)
	}
	job := before.Jobs[0]
	owner := ownerPattern.FindStringSubmatch(job.OwnerID)
	if job.ID != "run" || job.Phase != "running" || !job.Activated || job.ProcessID != 123 || job.Provider != "claude" || owner == nil || owner[1] != fmt.Sprint(os.Getpid()) || job.SessionID == "" {
		t.Fatalf("job: %+v", job)
	}
	if job.Continue == nil || job.Continue.State != "running" || job.Continue.Turn != 1 || job.Continue.MaxTurns != 3 {
		t.Fatalf("the job's follow-up state: %+v", job.Continue)
	}
	a.close()
	h.awaitNoController()

	// Nobody is connected. The provider writes and finishes its turn.
	m.session.events <- codingagent.Event{Text: "Written while nobody was connected", Status: "completed"}
	m.awaitState("parked")
	// Not lost: it is in the run record the job's own loop keeps, which is where a
	// reconnecting host reads it. The server holds nothing back for a client.
	if _, provider, output := m.savedRun(); provider != "running" || !strings.Contains(output, "Written while nobody was connected") {
		t.Fatalf("the output produced while disconnected is not in the record (%s):\n%s", provider, output)
	}
	if h.hasExited() || len(m.runner.Jobs()) != 1 {
		t.Fatal("the job or the server ended with its client")
	}

	b := h.dial()
	// A connected client is sent nothing it did not ask for, however much the
	// job writes: there is no backlog to overflow.
	b.expectSilence(150 * time.Millisecond)
	after := socketResult[ServeStatusResult](t, b.call(OpServeStatus, nil))
	if len(after.Jobs) != 1 || after.Jobs[0].OwnerID != job.OwnerID || after.Jobs[0].SessionID != job.SessionID || after.OwnerID != before.OwnerID {
		t.Fatalf("the reconnecting client sees another job or server: %+v", after)
	}
	if got := after.Jobs[0].Continue; got == nil || got.State != "parked" || got.Turn != 1 {
		t.Fatalf("the job the client adopts is not parked on turn 1: %+v", got)
	}
	res := socketResult[ManagedContinueResult](t, b.call(opManagedContinue, map[string]string{"id": "run", "request_id": "adopted", "message": "Carry on from where you stopped"}))
	if res.State != "delivered" || res.Turn != 2 {
		t.Fatalf("the follow-up from the adopting client: %+v", res)
	}
	if got := m.session.offered(); len(got) != 1 || got[0] != "Carry on from where you stopped" || m.session.turns.Load() != 1 {
		t.Fatalf("the provider was offered %q after %d StartTurn: the follow-up did not reach the same session", got, m.session.turns.Load())
	}
}

func TestSocketShutdownLeavesManagedJobsToTheirOwner(t *testing.T) {
	m := continuable(t, func(s *managedTestSession) { s.leaveOpen = true })
	m.startContinuable(1)
	m.awaitTurn(1)
	h := socketForLifecycle(t, m, nil)
	c := h.dial()
	c.call(OpServeStatus, nil)
	path := h.srv.Path()
	if err := h.stop(); err != nil {
		t.Fatalf("Serve returned %v on a signal", err)
	}
	c.expectClosed()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket file outlived the server: %v", err)
	}
	// Ending the server is not ending the run: the caller owns the runner.
	if len(m.runner.Jobs()) != 1 {
		t.Fatal("serving stopped a managed job")
	}
}

func TestSocketStopAllEndsEveryJobAndSaysWhichEnded(t *testing.T) {
	m := continuable(t, func(s *managedTestSession) { s.leaveOpen = true })
	m.startContinuable(1)
	m.awaitTurn(1)
	h := socketForLifecycle(t, m, nil)
	c := h.dial()

	for name, params := range map[string]any{
		"negative wait":   map[string]int{"wait_ms": -1},
		"wait too long":   map[string]int{"wait_ms": int(maxStopAllWait.Milliseconds()) + 1},
		"unknown member":  map[string]bool{"everything": true},
		"not an object":   []int{1},
		"trailing member": map[string]any{"wait_ms": 10, "x": nil},
	} {
		if resp := c.call(OpServeStopAll, params); resp.OK || resp.Error.Code != ErrBadRequest {
			t.Fatalf("%s was accepted: %+v", name, resp)
		}
	}
	if len(m.runner.Jobs()) != 1 {
		t.Fatal("a refused stop_all stopped something")
	}

	res := socketResult[ServeStopAllResult](t, c.call(OpServeStopAll, map[string]int{"wait_ms": 5000}))
	if len(res.Stopped) != 1 || res.Stopped[0] != "run" || len(res.Pending) != 0 {
		t.Fatalf("stop_all: %+v", res)
	}
	run, _ := m.finished()
	if run.State != "exited" || run.ProviderState != "interrupted" {
		t.Fatalf("a stopped job ended as %s/%s", run.State, run.ProviderState)
	}
	status := socketResult[ServeStatusResult](t, c.call(OpServeStatus, nil))
	if len(status.Jobs) != 0 {
		t.Fatalf("a stopped job is still listed: %+v", status.Jobs)
	}
	// With nothing to stop the answer is two empty lists, not two nulls.
	resp := c.call(OpServeStopAll, nil)
	if !resp.OK || !strings.Contains(string(resp.Result), `"stopped":[]`) || !strings.Contains(string(resp.Result), `"pending":[]`) {
		t.Fatalf("stop_all with nothing running: %s", resp.Result)
	}
	// It did not close the runner or the server.
	if !c.call(OpServeStatus, nil).OK {
		t.Fatal("the server stopped answering after stop_all")
	}
}

func TestSocketStopAllReportsAJobThatHasNotEndedAsPending(t *testing.T) {
	// A provider whose close takes longer than the wait: the job is cancelled
	// at once and ends later.
	m := continuable(t, func(s *managedTestSession) {
		s.leaveOpen = true
		s.closeDelay = 600 * time.Millisecond
	})
	m.startContinuable(1)
	m.awaitTurn(1)
	h := socketForLifecycle(t, m, nil)
	c := h.dial()
	res := socketResult[ServeStopAllResult](t, c.call(OpServeStopAll, map[string]int{"wait_ms": 50}))
	if len(res.Pending) != 1 || res.Pending[0] != "run" || len(res.Stopped) != 0 {
		t.Fatalf("a job still closing was reported as stopped: %+v", res)
	}
	m.finished()
}

func TestSocketExitsWhenIdleButNotWhileAClientOrAJobIsThere(t *testing.T) {
	setForTest(t, &disconnectGrace, 50*time.Millisecond)
	m := continuable(t, func(s *managedTestSession) { s.leaveOpen = true })
	m.startContinuable(1)
	m.awaitTurn(1)
	idle := 200 * time.Millisecond
	h := socketForLifecycle(t, m, func(cfg *SocketConfig) { cfg.IdleExit = idle })
	path := h.srv.Path()

	// A live job with no client keeps it up well past the idle period.
	time.Sleep(4 * idle)
	if h.hasExited() {
		t.Fatalf("the server exited with a live managed job:\n%s", h.log.String())
	}
	// So does a client with no job.
	m.stop()
	m.finished()
	c := h.dial()
	c.call(OpServeStatus, nil)
	time.Sleep(4 * idle)
	if h.hasExited() {
		t.Fatalf("the server exited with a client attached:\n%s", h.log.String())
	}
	// Neither: it exits, closes the socket and says why.
	c.close()
	h.await()
	if h.err != nil {
		t.Fatalf("an idle exit is not an error: %v", h.err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket file outlived an idle exit: %v", err)
	}
	if !strings.Contains(h.log.String(), "no client and no live managed job") {
		t.Fatalf("the idle exit was not explained:\n%s", h.log.String())
	}
}

func TestSocketThatNobodyEverConnectsToStillExitsWhenIdle(t *testing.T) {
	h := startSocketServer(t, SocketConfig{Options: hostOpts(), IdleExit: 100 * time.Millisecond})
	h.await()
	if h.err != nil {
		t.Fatal(h.err)
	}
}

func TestSocketRejectsAnIdlePeriodItCannotHonour(t *testing.T) {
	for _, idle := range []time.Duration{0, -time.Second, MaxIdleExit + time.Second} {
		_, err := ListenSocket(t.Context(), SocketConfig{Path: filepath.Join(socketTestDir(t), "s"), Options: hostOpts(), IdleExit: idle})
		if err == nil {
			t.Fatalf("an idle period of %s was accepted", idle)
		}
	}
}

func TestSocketRefusesAConfigurationThatCannotServeBeforeBinding(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "s")
	_, err := ListenSocket(t.Context(), SocketConfig{Path: path, Options: Options{Modules: []Module{WorkbenchModule{}}}, IdleExit: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "configure host plane") {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a socket was bound for a server that cannot serve: %v", statErr)
	}
}

// stdio is unchanged: a server built without the socket layer advertises none
// of its ops, and the lane of every op it always served is as it was.
func TestStdioServerDoesNotServeTheSocketOps(t *testing.T) {
	for _, op := range opsOf(t, hostOpts()) {
		if strings.HasPrefix(op, "serve.") {
			t.Fatalf("a stdio server advertises %s", op)
		}
	}
	resp := roundTrip(t, hostOpts(), Request{ID: "s", Op: OpServeStatus})[0]
	wantResponseCode(t, resp, ErrBadRequest)
	if got := laneFor(OpHello); got != "" {
		t.Fatalf("hello moved to lane %q", got)
	}
	if laneFor(OpServeStopAll) == laneFor("work.runs.managed.prepare") {
		t.Fatal("serve.stop_all shares the managed lane, behind a prepare")
	}
	if deadlineFor(OpServeStopAll) <= maxStopAllWait {
		t.Fatalf("serve.stop_all's deadline %s does not outlast its longest wait %s", deadlineFor(OpServeStopAll), maxStopAllWait)
	}
}
