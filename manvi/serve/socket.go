package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Socket-served mode: `manvi serve --listen <path>`.
//
// The stdio server lives and dies with the program that spawned it, and so do
// the managed jobs it hosts. A host that wants to leave jobs running when it
// quits, and find them again when it starts, needs a server that does not
// depend on it. This is that server: the same framed NDJSON protocol, spoken
// over a unix socket, with the managed runner (and the provider processes it
// owns) kept across the clients that come and go.
//
// What a client sees is exactly what a stdio host sees: one New(conn, opts)
// server per connection, so each connection has its own chat state and lane
// pool and the wire is unchanged. What is shared is what the host-plane modules
// close over: the managed runner and the store. A connection ending cancels only
// that connection's requests; managed jobs run on their own contexts and carry
// on.
//
// Output a job produces while no client is attached is not buffered here. The
// server never pushes a line the host did not ask for (protocol.go), and a job's
// text and tool snapshot are checkpointed into its run record by its own drive
// loop (runs.protocol), bounded by maxManagedOutput, whether or not anyone is
// connected. A reconnecting host reads the record. There is no in-memory backlog
// for a disconnected client to overflow.
//
// Security model.
//
//   - The socket lives in a directory this process owns and only it can enter
//     (mode 0700, owned by the effective uid), and is itself mode 0600. An
//     existing directory is checked, never repaired: chmod-ing a directory the
//     operator pointed us at would change something that is not ours.
//   - The socket path and its directory must not be symbolic links, and the path
//     must name a socket (or nothing). A regular file at the path is never
//     removed. A socket at the path is removed only when nothing answers on it.
//   - Every accepted connection is checked against the kernel's record of the
//     peer's uid (SO_PEERCRED / LOCAL_PEERCRED) and closed unless it is this
//     process's own uid. A platform that cannot report it refuses to serve:
//     authenticating nobody is not a fallback.
//   - The peer uid is the only authentication. Every process running as this user
//     can drive the server, as every such process can already drive a stdio
//     server it spawned or read the profile database the server writes.
//
// Controlling client. Exactly one connection controls the server at a time:
// only it can send ops, so there are never two writers.
//
//   - A connection that arrives while the slot is free becomes the controller at
//     once; no handshake is needed, and a stdio host's bytes work unchanged.
//   - A connection that arrives while there is a controller is a waiting
//     connection. It is answered E_CONTROLLER_BUSY (retryable) to everything
//     except serve.status, which reports who the server is and which jobs are
//     live, and serve.takeover. It is closed after waitingIdle with no request.
//   - The controller leaving frees the slot: closing the connection (or the
//     process dying, which the kernel closes for it) is a handover, and the next
//     client to connect controls. The slot is not free until the previous
//     controller's connection has been fully served out, so a new client never
//     runs beside a request the old one left in flight; requests it left are
//     given disconnectGrace to answer a half-closed peer and are then cancelled.
//   - serve.takeover is the explicit replacement of a live controller: the
//     current controller is cancelled and its connection closed, the server waits
//     (bounded by takeoverWait) for that to finish, and only then does the caller
//     become the controller. If it does not finish in time the takeover is
//     refused, retryably, and nothing changes hands: failing closed beats two
//     writers. Two takeovers racing are decided by whichever claims the slot
//     first; the other is told the server is busy.
//
// Jobs keep running while no client is attached. The server ends only on
// SIGINT/SIGTERM, on an idle exit (no client and no live managed job for
// IdleExit), or on a listener failure; a client disconnecting is not one of them.
// serve.stop_all is the explicit way to end every job.

// MaxIdleExit bounds the idle period. An unbounded idle server is a daemon
// nobody asked for.
const MaxIdleExit = 24 * time.Hour

const (
	socketFileMode = 0o600
	socketDirMode  = 0o700
	// maxSocketPath keeps the address inside sockaddr_un.sun_path, which is 104
	// bytes on macOS and 108 on Linux, NUL included.
	maxSocketPath = 100
	// maxSocketConns bounds the connections held at once: the controller and the
	// waiting. Past it a connection is closed without a word.
	maxSocketConns = 8
	// maxWaitingLine bounds a line from a connection that is not the controller;
	// the only requests it can make are tiny.
	maxWaitingLine = 4 << 10
	// shutdownWait bounds how long shutdown waits for connection handlers.
	shutdownWait = 10 * time.Second
)

var (
	// disconnectGrace is how long the requests a vanished controller left in
	// flight may still be answered before they are cancelled. Tests shorten it.
	disconnectGrace = 5 * time.Second
	// takeoverWait bounds how long a takeover waits for the previous controller to
	// be served out.
	takeoverWait = 10 * time.Second
	// socketWriteStall bounds one write to a client. A controller that stops
	// reading would otherwise hold the slot, and a lane, for ever.
	socketWriteStall = 30 * time.Second
	// waitingIdle is how long a waiting connection may sit without a request.
	waitingIdle = 30 * time.Second
)

var errIdleExit = errors.New("no client and no live managed job for the idle period")

// SocketConfig configures ListenSocket.
type SocketConfig struct {
	// Path is the absolute path of the socket.
	Path string
	// Options configures the server each connection gets. Its Modules are shared
	// by every connection, so the runners they hold outlive any one of them. It
	// must not contain a ControlModule: the socket server adds its own.
	Options Options
	// Managed is the runner whose jobs keep the server alive and which
	// serve.status and serve.stop_all report on. Nil for a server that hosts none.
	Managed *ManagedRunner
	// IdleExit is how long the server waits with no client and no live managed
	// job before it exits. Required, positive, at most MaxIdleExit.
	IdleExit time.Duration
	// Log receives one line per notable event (a refused peer, a takeover, the
	// reason the server stopped). Nil discards them.
	Log io.Writer

	// peer reads a connection's peer uid; tests replace it.
	peer func(*net.UnixConn) (uint32, error)
}

// SocketServer is a bound socket and the controller state of the server on it.
type SocketServer struct {
	cfg      SocketConfig
	listener *net.UnixListener
	path     string
	made     os.FileInfo
	owner    string
	pid      int
	uid      uint32
	opts     Options

	mu      sync.Mutex
	ctrl    *controller
	conns   map[net.Conn]struct{}
	closing bool
	wg      sync.WaitGroup
}

// controller is the one connection that controls the server.
type controller struct {
	conn   net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// ListenSocket checks the path and its directory, binds the socket and returns
// the server. Nothing is accepted until Serve. Every refusal happens here, before
// anything exists on disk that the refusal would have to clean up. ctx bounds the
// one connection it makes, to ask whether a socket already at the path is served.
func ListenSocket(ctx context.Context, cfg SocketConfig) (*SocketServer, error) {
	if !peerCredSupported {
		return nil, errors.New("socket-served mode is not available on this platform: it cannot check who connects, and a socket that authenticates nobody is not offered")
	}
	if cfg.peer == nil {
		cfg.peer = peerUID
	}
	if cfg.IdleExit <= 0 || cfg.IdleExit > MaxIdleExit {
		return nil, fmt.Errorf("the idle exit is between 1ns and %s, not %s", MaxIdleExit, cfg.IdleExit)
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	uid := uint32(os.Geteuid()) // #nosec G115 -- peerCredSupported platforms have no negative uid
	path, err := checkedSocketPath(cfg.Path, uid)
	if err != nil {
		return nil, err
	}
	s := &SocketServer{cfg: cfg, path: path, pid: os.Getpid(), uid: uid, conns: map[net.Conn]struct{}{}}
	s.opts = cfg.Options
	s.opts.Modules = append(append([]Module(nil), cfg.Options.Modules...), ControlModule{Control: s})
	// The options are validated once, here: a configuration that cannot serve a
	// request would otherwise be discovered by each client, one connection at a time.
	if trial := New(io.Discard, s.opts); trial.configErr != nil {
		return nil, fmt.Errorf("configure host plane: %w", trial.configErr)
	}
	if s.owner, err = managedOwnerID(); err != nil {
		return nil, err
	}
	if err := clearStaleSocket(ctx, path, uid); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// The unlink on close would remove whatever is at the path, which by then may
	// not be our file; shutdown removes it only after checking.
	listener.SetUnlinkOnClose(false)
	s.listener = listener
	if err := s.secureSocket(); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return s, nil
}

// secureSocket sets the socket's mode and records the file so shutdown can tell
// it from a successor's. The directory is 0700, so the window between bind and
// chmod is not reachable by anyone else; this makes the socket's own mode right
// regardless of the umask.
func (s *SocketServer) secureSocket() error {
	if err := os.Chmod(s.path, socketFileMode); err != nil {
		return fmt.Errorf("restrict %s: %w", s.path, err)
	}
	info, err := os.Lstat(s.path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", s.path, err)
	}
	owner, ok := fileOwner(info)
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != socketFileMode || !ok || owner != s.uid {
		_ = os.Remove(s.path)
		return fmt.Errorf("%s is not the 0600 socket this process just bound", s.path)
	}
	s.made = info
	return nil
}

// checkedSocketPath validates the requested path and its directory and returns
// the path to bind: the directory resolved, the socket's own name unchanged.
func checkedSocketPath(raw string, uid uint32) (string, error) {
	if raw == "" || !filepath.IsAbs(raw) || strings.ContainsRune(raw, 0) {
		return "", errors.New("the socket path is an absolute path")
	}
	path := filepath.Clean(raw)
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%s does not name a socket file", raw)
	}
	if err := checkSocketDir(dir, uid); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	path = filepath.Join(resolved, name)
	if len(path) > maxSocketPath {
		return "", fmt.Errorf("the socket path is %d bytes; a unix socket address holds at most %d: use a shorter directory", len(path), maxSocketPath)
	}
	return path, nil
}

// checkSocketDir makes sure dir is a real directory, owned by this uid and
// closed to everyone else, creating it (the last component only, 0700) when it
// is absent. It never changes an existing directory.
func checkSocketDir(dir string, uid uint32) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if mkErr := os.Mkdir(dir, socketDirMode); mkErr != nil {
			return fmt.Errorf("create %s: %w", dir, mkErr)
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", dir, err)
	}
	owner, known := fileOwner(info)
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symbolic link: the socket's directory must be a real directory", dir)
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	case !known || owner != uid:
		return fmt.Errorf("%s is not owned by this user", dir)
	case info.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%s is accessible to other users (mode %04o): make it 0700 or choose another directory", dir, info.Mode().Perm())
	}
	return nil
}

// clearStaleSocket removes a socket nothing answers on, and refuses everything
// else that is in the way: a link, a file that is not a socket, a socket someone
// else owns, a socket that is being served, or one it cannot tell about.
func clearStaleSocket(ctx context.Context, path string, uid uint32) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	owner, known := fileOwner(info)
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symbolic link; it is not followed or replaced", path)
	case info.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("%s exists and is not a socket; it is not replaced", path)
	case !known || owner != uid:
		return fmt.Errorf("%s is a socket this user does not own; it is not replaced", path)
	}
	probe, dialErr := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
	switch {
	case dialErr == nil:
		_ = probe.Close()
		return fmt.Errorf("%s is already being served", path)
	case !errors.Is(dialErr, syscall.ECONNREFUSED):
		// Anything but a refusal might be a server that is busy or starting. A
		// socket is removed only when that is known not to be so.
		return fmt.Errorf("%s may be served by another process; it is not replaced: %w", path, dialErr)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove the stale socket %s: %w", path, err)
	}
	return nil
}

// Path is the socket's path as bound.
func (s *SocketServer) Path() string { return s.path }

// OwnerID is the server's identity, in the form a managed claim records.
func (s *SocketServer) OwnerID() string { return s.owner }

// Status implements ServerControl for a controlling connection.
func (s *SocketServer) Status() ServeStatusResult { return s.status(true) }

func (s *SocketServer) status(controller bool) ServeStatusResult {
	return ServeStatusResult{
		Protocol:   ProtocolVersion,
		OwnerID:    s.owner,
		ProcessID:  s.pid,
		Controller: controller,
		Jobs:       s.cfg.Managed.Jobs(),
		IdleExitMS: s.cfg.IdleExit.Milliseconds(),
	}
}

// StopAll implements ServerControl.
func (s *SocketServer) StopAll(ctx context.Context, wait time.Duration) ServeStopAllResult {
	stopped, pending := s.cfg.Managed.StopAll(ctx, wait)
	return ServeStopAllResult{Stopped: stopped, Pending: pending}
}

func (s *SocketServer) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.cfg.Log, "manvi serve: "+format+"\n", args...)
}

// Serve accepts clients until ctx ends, the idle period passes, or the listener
// fails, then closes the socket and removes its file. Managed jobs are not
// touched: the caller owns the runner and closes it.
func (s *SocketServer) Serve(ctx context.Context) error {
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	acceptErr := make(chan error, 1)
	go func() { acceptErr <- s.acceptLoop(ctx) }()
	go s.watchIdle(ctx, stop)
	var err error
	select {
	case <-ctx.Done():
	case err = <-acceptErr:
		stop(err)
	}
	s.shutdown()
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errIdleExit):
		s.logf("exiting: %v", cause)
	case err != nil:
		s.logf("exiting: %v", err)
	}
	return err
}

func (s *SocketServer) acceptLoop(ctx context.Context) error {
	failures := 0
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			// The listener is closed by shutdown, which is how this loop is told to
			// stop; any other failure is retried a bounded number of times.
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			if failures++; failures > 50 {
				return fmt.Errorf("accept on %s: %w", s.path, err)
			}
			select {
			case <-time.After(time.Duration(failures) * 20 * time.Millisecond):
				continue
			case <-ctx.Done():
				return nil
			}
		}
		failures = 0
		s.mu.Lock()
		if s.closing || len(s.conns) >= maxSocketConns {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.handle(ctx, conn)
	}
}

// watchIdle ends the server once there has been no client and no live managed job
// for IdleExit. A client or a job resets the clock.
func (s *SocketServer) watchIdle(ctx context.Context, stop context.CancelCauseFunc) {
	tick := min(max(s.cfg.IdleExit/4, 5*time.Millisecond), time.Second)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	var idleSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Jobs are counted before the lock: they can only be added by a client,
		// and a client holds a connection, which the lock checks.
		jobs := s.cfg.Managed.LiveJobCount()
		s.mu.Lock()
		idle := len(s.conns) == 0 && jobs == 0
		due := idle && !idleSince.IsZero() && time.Since(idleSince) >= s.cfg.IdleExit
		if due {
			// Under the same lock as the check, so a client that arrives after it
			// is refused rather than accepted into a server that is leaving.
			s.closing = true
		}
		s.mu.Unlock()
		switch {
		case due:
			stop(errIdleExit)
			return
		case !idle:
			idleSince = time.Time{}
		case idleSince.IsZero():
			idleSince = time.Now()
		}
	}
}

// shutdown stops accepting, ends every connection and removes the socket file.
func (s *SocketServer) shutdown() {
	s.mu.Lock()
	s.closing = true
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	ctrl := s.ctrl
	s.mu.Unlock()
	_ = s.listener.Close()
	if ctrl != nil {
		ctrl.cancel()
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	finished := make(chan struct{})
	go func() { s.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(shutdownWait):
		s.logf("%d connection handlers had not finished after %s; leaving them to the process exit", len(conns), shutdownWait)
	}
	// Only the file this server made: a successor may already be bound at the path.
	if info, err := os.Lstat(s.path); err == nil && os.SameFile(info, s.made) {
		_ = os.Remove(s.path)
	}
}

// handle serves one accepted connection from its first byte to its last.
func (s *SocketServer) handle(ctx context.Context, conn *net.UnixConn) {
	defer s.wg.Done()
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()
	uid, err := s.cfg.peer(conn)
	if err != nil {
		s.logf("refused a connection whose peer could not be identified: %v", err)
		return
	}
	if uid != s.uid {
		s.logf("refused a connection from uid %d", uid)
		return
	}
	reader := bufio.NewReaderSize(conn, 64<<10)
	ctrl := s.claim(ctx, conn)
	if ctrl == nil {
		if ctrl = s.serveWaiting(ctx, conn, reader); ctrl == nil {
			return
		}
	}
	s.control(ctrl, reader)
}

// claim makes conn the controller if the slot is free.
func (s *SocketServer) claim(ctx context.Context, conn net.Conn) *controller {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctrl != nil || s.closing {
		return nil
	}
	return s.claimLocked(ctx, conn)
}

func (s *SocketServer) claimLocked(ctx context.Context, conn net.Conn) *controller {
	cctx, cancel := context.WithCancel(ctx)
	c := &controller{conn: conn, ctx: cctx, cancel: cancel, done: make(chan struct{})}
	s.ctrl = c
	return c
}

// control serves the controlling client until it is gone, then frees the slot.
// The slot is freed only after Serve has returned, which is after every request
// the client left in flight has been answered or cancelled.
func (s *SocketServer) control(c *controller, reader *bufio.Reader) {
	var grace *time.Timer
	var mu sync.Mutex
	// Read here, not where the stream ends: that is a goroutine Serve does not wait
	// for, and the knob is a variable only so that a test can shorten it.
	wait := disconnectGrace
	watched := &endWatch{r: reader, onEnd: func() {
		// The stream is over, so the requests still running have nobody to
		// answer unless the peer only half-closed. Give it the grace, then cancel.
		mu.Lock()
		defer mu.Unlock()
		grace = time.AfterFunc(wait, c.cancel)
	}}
	srv := New(stallWriter{c.conn}, s.opts)
	if err := srv.Serve(c.ctx, watched); err != nil && c.ctx.Err() == nil {
		s.logf("the controlling client's stream failed: %v", err)
	}
	mu.Lock()
	if grace != nil {
		grace.Stop()
	}
	mu.Unlock()
	c.cancel()
	s.mu.Lock()
	if s.ctrl == c {
		s.ctrl = nil
	}
	s.mu.Unlock()
	close(c.done)
}

// serveWaiting answers a connection that is not the controller. It returns the
// controller this connection has become by serve.takeover, or nil once the
// connection is to be closed.
func (s *SocketServer) serveWaiting(ctx context.Context, conn *net.UnixConn, reader *bufio.Reader) *controller {
	for {
		if err := conn.SetReadDeadline(time.Now().Add(waitingIdle)); err != nil {
			return nil
		}
		line, _, err := readLine(reader, maxWaitingLine)
		if err != nil || ctx.Err() != nil {
			return nil
		}
		// Cleared before anything is answered, so a connection that becomes the
		// controller is not left with the waiting connection's idle deadline.
		if conn.SetReadDeadline(time.Time{}) != nil {
			return nil
		}
		var req Request
		if json.Unmarshal(line, &req) != nil || req.ID == "" {
			s.reply(conn, Response{Error: badRequest("a waiting connection sends one request per line with an id")})
			continue
		}
		switch req.Op {
		case OpServeStatus:
			s.replyResult(conn, req.ID, s.status(false))
		case OpServeTakeover:
			c, tookOver, e := s.takeover(ctx, conn)
			if e != nil {
				s.reply(conn, Response{ID: req.ID, Error: e})
				continue
			}
			s.replyResult(conn, req.ID, ServeTakeoverResult{Controller: true, TookOver: tookOver})
			return c
		default:
			s.reply(conn, Response{ID: req.ID, Error: &Error{
				Code:      ErrControllerBusy,
				Retryable: true,
				Message: "another client controls this server: reconnect when it has gone, or send " + OpServeTakeover +
					" to replace it; " + OpServeStatus + " says who and what is running",
			}})
		}
	}
}

// takeover makes conn the controller, replacing the current one. The previous
// controller is cancelled and its connection closed first, and the slot is
// claimed only after that connection has been served out, so the two are never
// both live. Refused, retryably, if that does not finish within takeoverWait or
// if another takeover claimed the slot first.
func (s *SocketServer) takeover(ctx context.Context, conn net.Conn) (*controller, bool, *Error) {
	s.mu.Lock()
	old := s.ctrl
	if s.closing {
		s.mu.Unlock()
		return nil, false, &Error{Code: ErrControllerBusy, Message: "the server is shutting down", Retryable: false}
	}
	if old != nil {
		old.cancel()
		_ = old.conn.Close()
	}
	s.mu.Unlock()
	if old != nil {
		select {
		case <-old.done:
		case <-time.After(takeoverWait):
			return nil, false, &Error{Code: ErrControllerBusy, Retryable: true,
				Message: fmt.Sprintf("the controlling client was cancelled but its requests had not ended after %s; nothing changed hands", takeoverWait)}
		case <-ctx.Done():
			return nil, false, &Error{Code: ErrControllerBusy, Message: "the server is shutting down"}
		}
		s.logf("a new client took over from the controlling client")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctrl != nil || s.closing {
		return nil, false, &Error{Code: ErrControllerBusy, Retryable: true, Message: "another client took the slot first"}
	}
	return s.claimLocked(ctx, conn), old != nil, nil
}

func (s *SocketServer) reply(conn net.Conn, resp Response) {
	encoded, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = stallWriter{conn}.Write(append(encoded, '\n'))
}

func (s *SocketServer) replyResult(conn net.Conn, id string, result any) {
	encoded, err := json.Marshal(result)
	if err != nil {
		s.reply(conn, Response{ID: id, Error: &Error{Code: ErrInternal, Message: fmt.Sprintf("encoding the answer: %v", err)}})
		return
	}
	s.reply(conn, Response{ID: id, OK: true, Result: encoded})
}

// endWatch tells its owner when the stream under it has ended or failed.
type endWatch struct {
	r     io.Reader
	once  sync.Once
	onEnd func()
}

func (e *endWatch) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil {
		e.once.Do(e.onEnd)
	}
	return n, err
}

// stallWriter writes to a client with a deadline on every write, so one that has
// stopped reading ends the connection instead of holding it.
type stallWriter struct{ conn net.Conn }

func (w stallWriter) Write(p []byte) (int, error) {
	if err := w.conn.SetWriteDeadline(time.Now().Add(socketWriteStall)); err != nil {
		return 0, err
	}
	return w.conn.Write(p)
}
