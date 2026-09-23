package runtime

// Real TCP, both directions, as plain int handles.
//
// Inbound (io.net.listen): listen/accept mint listener and connection
// handles, gated separately from io.net (see internal/ir/build.go's
// builtinCapability doc) and restricted to a daemon body specifically
// (checkDaemonOnlyBuiltins) because accept() is genuinely, indefinitely
// blocking, and so is readBytes on an accepted connection by default — a
// daemon's own goroutine is the one place in this runtime that is safe to
// block forever, since nothing ever joins it and it holds no lock (see
// runtime/daemon.go's doc).
//
// Outbound (io.net): connect(host, port) dials a TCP connection from any
// proc that declares `uses io.net`. Such a proc may run under an action's
// store lock, so a connect-minted connection is ALWAYS bounded: the dial and
// every later readBytes/writeBytes carry a deadline (connectDefaultTimeout,
// adjustable per connection with setTimeoutMs but never removable), so no
// peer can hold an ordinary proc — or the lock above it — forever. This is
// the primitive protocols are written on in fct itself (an HTTP/1.1 client
// is fct/selfhost/http_client.fct), where httpGet/httpPost are a fixed,
// headerless, status-less text exchange.
//
// readBytes/writeBytes/closeConn/setTimeoutMs/connError work on a
// connection handle whichever way it was minted; they are satisfied by
// either io.net or io.net.listen (see build.go's netConnCap).
//
// Transport failures are values, not aborts. A client has to be able to
// report "that instance is unreachable" as data (a FacetqlError::Transport,
// say) rather than having its whole proc — and the action that called it —
// torn down, so: a refused/timed-out dial still mints a handle, in a failed
// state; a read that times out or errors returns [] like a clean close; a
// write that fails returns false; and connError(c) says which, if any, of
// those happened ("" while the connection is healthy or merely closed by the
// peer). Only a programming error — an unknown handle, a bad port, a byte
// out of range — aborts.
//
// A Listener/Conn is, deliberately, just an opaque int handle — the exact
// same move runtime/channel.go's channelRegistry already makes for a channel
// value, for the same reason: an int is already a legal proc/daemon
// parameter, `let` local, array element, and builtin argument, so a handle
// gets all of that for free just by being one, with no new type anywhere in
// the compiler's type system (see internal/ir/build.go's inferProcType,
// "listen"/"accept" cases).
import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// connectDefaultTimeout bounds a connect-minted connection's dial and each of
// its reads and writes until setTimeoutMs says otherwise.
const connectDefaultTimeout = 10 * time.Second

// maxConnTimeoutMs is the largest per-operation deadline setTimeoutMs accepts
// (ten minutes): large enough for any real exchange, small enough that a
// bounded connection stays bounded.
const maxConnTimeoutMs = 600_000

// netConn is one connection handle's state. c is nil when the dial failed:
// the handle still exists so the failure can be read back with connError.
// timeout 0 means "no deadline", which only an accepted connection starts
// with. err is the first transport failure seen on this connection.
type netConn struct {
	c        net.Conn
	outbound bool
	timeout  time.Duration
	err      string
	// eof: a read saw the peer's clean close. With err, what connOpen
	// reports: nothing more will ever arrive.
	eof bool
}

// netRegistry backs listen/accept/readBytes/writeBytes/closeConn, mapping the
// int handles those builtins hand back to real Go net.Listener/net.Conn
// values. Its own mutex — deliberately never s.mu (the durable-store lock) —
// guards only the maps' own bookkeeping (minting a handle, looking one up),
// exactly like channelRegistry's mu: accept()'s and readBytes'/writeBytes'
// real blocking I/O always happens AFTER this mutex is released, on the
// looked-up net.Listener/net.Conn directly, so one goroutine blocked in
// Accept() or Read() never holds up another goroutine minting or looking up
// a different handle, let alone the store lock (which this file never
// touches at all).
type netRegistry struct {
	mu        sync.Mutex
	next      int
	listeners map[int]*netListener
	conns     map[int]*netConn
}

// netListener is one listener handle's state. ln is nil when binding failed:
// the handle still exists so the failure can be read back with listenError
// (connect's own shape — see this file's doc — applied to listen). err is
// that failure; closed is set by closeListener.
type netListener struct {
	ln     net.Listener
	err    string
	closed bool
}

func newNetRegistry() *netRegistry {
	return &netRegistry{listeners: map[int]*netListener{}, conns: map[int]*netConn{}}
}

// mintListener registers nl under a fresh handle.
func (r *netRegistry) mintListener(nl *netListener) int {
	r.mu.Lock()
	r.next++
	id := r.next
	r.listeners[id] = nl
	r.mu.Unlock()
	return id
}

// osErrorText describes a failed socket call the way the operating system
// does: the errno's own description and number ("Address already in use (os
// error 98)"), which is what C's strerror and Rust's io::Error print — and
// so what an operator's other tools already say for the same failure.
// Anything that is not an errno keeps Go's description.
func osErrorText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		msg := errno.Error()
		if msg != "" {
			msg = strings.ToUpper(msg[:1]) + msg[1:]
		}
		return fmt.Sprintf("%s (os error %d)", msg, int(errno))
	}
	return err.Error()
}

// mint registers nc under a fresh handle.
func (r *netRegistry) mint(nc *netConn) int {
	r.mu.Lock()
	r.next++
	id := r.next
	r.conns[id] = nc
	r.mu.Unlock()
	return id
}

// fail records the first transport failure on nc; later ones are
// consequences of it and would only obscure the cause.
func (r *netRegistry) fail(nc *netConn, msg string) {
	r.mu.Lock()
	if nc.err == "" {
		nc.err = msg
	}
	r.mu.Unlock()
}

// ioConnect implements `connect(host: text, port: int) -> int` (io.net):
// dials host:port over TCP within connectDefaultTimeout and mints a bounded
// connection handle. A dial that fails still returns a handle — see this
// file's doc — whose connError names the failure.
func (s *Server) ioConnect(host string, port int) (any, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("connect: invalid port %d (must be 1-65535)", port)
	}
	if host == "" {
		return nil, fmt.Errorf("connect: empty host")
	}
	nc := &netConn{outbound: true, timeout: connectDefaultTimeout}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	c, err := net.DialTimeout("tcp", addr, nc.timeout)
	if err != nil {
		nc.err = fmt.Sprintf("connect %s: %v", addr, err)
	} else {
		nc.c = c
	}
	return s.netConns.mint(nc), nil
}

// ioSetTimeoutMs implements `setTimeoutMs(c: int, ms: int) -> bool`: every
// later readBytes/writeBytes on c must finish within ms milliseconds or
// fail (see connError). The range is 1..maxConnTimeoutMs — there is no way
// to ask for "no deadline", so a connect-minted connection stays bounded.
func (s *Server) ioSetTimeoutMs(id int, ms int) (any, error) {
	if ms <= 0 || ms > maxConnTimeoutMs {
		return nil, fmt.Errorf("setTimeoutMs: %d ms is out of range (must be 1-%d)", ms, maxConnTimeoutMs)
	}
	nc, err := s.lookupConn(id, "setTimeoutMs")
	if err != nil {
		return nil, err
	}
	r := s.netConns
	r.mu.Lock()
	nc.timeout = time.Duration(ms) * time.Millisecond
	r.mu.Unlock()
	return true, nil
}

// ioConnError implements `connError(c: int) -> text`: the first transport
// failure on c (a failed dial, a read or write that errored or timed out),
// or "" when there has been none. A peer closing the connection cleanly is
// not a failure.
func (s *Server) ioConnError(id int) (any, error) {
	nc, err := s.lookupConn(id, "connError")
	if err != nil {
		return nil, err
	}
	r := s.netConns
	r.mu.Lock()
	msg := nc.err
	r.mu.Unlock()
	return msg, nil
}

// deadline is the absolute deadline for one operation on nc starting now, or
// the zero time (none) for an accepted connection nobody bounded.
func (r *netRegistry) deadline(nc *netConn) time.Time {
	r.mu.Lock()
	d := nc.timeout
	r.mu.Unlock()
	if d <= 0 {
		return time.Time{}
	}
	return time.Now().Add(d)
}

// describeNetErr renders a transport error for connError, naming a deadline
// expiry as the timeout it is.
func describeNetErr(who string, nc *netConn, err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Sprintf("%s: timed out after %dms", who, nc.timeout.Milliseconds())
	}
	return fmt.Sprintf("%s: %v", who, err)
}

// ioListen implements the `listen(port: int) -> int` builtin (io.net.listen):
// binds a TCP listener on port across every local interface and mints its
// handle. A bind that fails (the port is taken, the address is not this
// machine's, permission) is a value, not an abort: the handle is minted in a
// failed state and listenError(l) says why — exactly as a failed connect()
// mints a handle whose connError says why — so a server can answer its
// operator (fabricd exits 78 naming the address) rather than dying. An
// invalid port is still a programming error. The listener stays open until
// closeListener(l) (or process exit).
func (s *Server) ioListen(port int) (any, error) {
	return s.ioListenOn("listen", "", port)
}

// ioListenOn implements `listenOn(host: text, port: int) -> int`: listen()
// bound to one local address ("127.0.0.1", "::1", a hostname) instead of every
// interface — what a listener that must never be reachable off the machine
// (a control-plane port) needs. host "" is every interface, i.e. listen().
func (s *Server) ioListenOn(who, host string, port int) (any, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("%s: invalid port %d (must be 1-65535)", who, port)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return s.netConns.mintListener(&netListener{err: osErrorText(err)}), nil
	}
	return s.netConns.mintListener(&netListener{ln: ln}), nil
}

// ioListenError implements `listenError(l: int) -> text`: why l's bind
// failed, or "" for a listener that bound (closed or not).
func (s *Server) ioListenError(id int) (any, error) {
	nl, err := s.lookupListener(id, "listenError")
	if err != nil {
		return nil, err
	}
	return nl.err, nil
}

// ioCloseListener implements `closeListener(l: int) -> bool`: stops l
// accepting — the port is released, so a client connecting after this is
// refused by the operating system, as it is by a server that dropped its
// listener. An accept() parked on l, in any task, returns at once with a
// connection handle in a failed state whose connError is "accept: the
// listener is closed", and so does every later accept on it: the clean end
// of an accept loop. Answers false for a listener that never bound or is
// already closed.
func (s *Server) ioCloseListener(id int) (any, error) {
	nl, err := s.lookupListener(id, "closeListener")
	if err != nil {
		return nil, err
	}
	r := s.netConns
	r.mu.Lock()
	if nl.ln == nil || nl.closed {
		r.mu.Unlock()
		return false, nil
	}
	nl.closed = true
	r.mu.Unlock()
	nl.ln.Close()
	return true, nil
}

// lookupListener resolves a listener handle, or a clean error naming what
// is wrong with it.
func (s *Server) lookupListener(id int, who string) (*netListener, error) {
	r := s.netConns
	r.mu.Lock()
	nl, ok := r.listeners[id]
	_, isConn := r.conns[id]
	r.mu.Unlock()
	if !ok {
		if isConn {
			return nil, fmt.Errorf("%s: %d is a connection handle, not a listener", who, id)
		}
		return nil, fmt.Errorf("%s: listener %d does not exist", who, id)
	}
	return nl, nil
}

// ioAccept implements the `accept(l: int) -> int` builtin: blocks until a
// client connects to l (or l is closed / the process exits), then mints the
// new connection's own handle. The lookup of l is the only part of this call
// under the registry's mutex — the real Accept() call, which can block
// indefinitely, runs after that mutex is released, exactly as this file's
// doc promises.
//
// Accepting on a listener that never bound is a programming error (the
// program did not check listenError) and aborts, naming the bind's failure.
// Once the listener is closed, or when the operating system refuses one
// accept (too many open files, say), accept answers a connection handle in
// a failed state whose connError says why — a value, as a failed connect
// is: an accept loop ends when connError(c) is "accept: the listener is
// closed", and can carry on past a transient failure.
func (s *Server) ioAccept(id int) (any, error) {
	nl, err := s.lookupListener(id, "accept")
	if err != nil {
		return nil, err
	}
	r := s.netConns
	if nl.ln == nil {
		return nil, fmt.Errorf("accept: listener %d never bound: %s", id, nl.err)
	}
	r.mu.Lock()
	closed := nl.closed
	r.mu.Unlock()
	if closed {
		return r.mint(&netConn{err: "accept: the listener is closed"}), nil
	}
	conn, err := nl.ln.Accept()
	if err != nil {
		r.mu.Lock()
		closed = nl.closed
		r.mu.Unlock()
		if closed || errors.Is(err, net.ErrClosed) {
			return r.mint(&netConn{err: "accept: the listener is closed"}), nil
		}
		return r.mint(&netConn{err: "accept: " + osErrorText(err)}), nil
	}
	return r.mint(&netConn{c: conn}), nil
}

// ioReadBytes implements the `readBytes(c: int, maxLen: int) -> [int]`
// builtin: blocks until at least one byte is available (or the peer closes
// the connection, or a transport error, or c's deadline — always present on a
// connect-minted connection — passes), reading up to maxLen bytes in one
// call — never more, possibly fewer, exactly like the underlying net.Conn.Read
// it wraps. The peer closing the connection is reported as a clean, empty
// result (`[]`, io.EOF), not an error: it is the ordinary, expected way a
// connection ends, precisely the shape a daemon's own read loop checks
// (`if len(data) == 0: break`) to notice and move on — the same "not every
// exceptional-looking outcome is an error" stance runtime/channel.go's recv
// already takes for an empty channel (there, a block; here, an end).
func (s *Server) ioReadBytes(id int, maxLen int) (any, error) {
	if maxLen <= 0 {
		return nil, fmt.Errorf("readBytes: maxLen must be positive, got %d", maxLen)
	}
	nc, err := s.lookupConn(id, "readBytes")
	if err != nil {
		return nil, err
	}
	if nc.c == nil {
		// A failed dial: nothing will ever arrive; connError says why.
		return []any{}, nil
	}
	r := s.netConns
	if err := nc.c.SetReadDeadline(r.deadline(nc)); err != nil {
		r.fail(nc, fmt.Sprintf("readBytes: %v", err))
		return []any{}, nil
	}
	buf := make([]byte, maxLen)
	n, err := nc.c.Read(buf)
	if err != nil && n == 0 {
		// io.EOF (clean close) or any other read error both end the
		// connection from the reader's point of view; there is nothing
		// further to read either way, so this is the same empty result a
		// graceful EOF gets. Anything but a clean EOF is also recorded, so
		// connError can tell "the peer finished" from "the peer vanished"
		// or "the deadline passed".
		if !errors.Is(err, io.EOF) {
			r.fail(nc, describeNetErr("readBytes", nc, err))
		} else {
			r.markEOF(nc)
		}
		return []any{}, nil
	}
	out := make([]any, n)
	for i := 0; i < n; i++ {
		out[i] = int(buf[i])
	}
	return out, nil
}

// ioWriteBytes implements the `writeBytes(c: int, data: [int]) -> bool`
// builtin: writes every byte in data to c, looping over net.Conn.Write until
// the whole buffer is sent (true) or a transport error or c's deadline stops
// it (false, recorded for connError — see this file's doc) (a single Write call
// is not guaranteed to consume the whole slice — the same reason
// runtime/io.go's file writes use os.WriteFile's own all-or-nothing
// guarantee, which a raw socket write has no equivalent of). Every element
// must already be a valid byte (0-255) — the same range this runtime's other
// byte-buffer producers (bytes(n), readBytes above) already guarantee, and
// the same defensive re-check runtime/io.go's ioWriteFileBytes already makes
// for its own byte-buffer argument.
func (s *Server) ioWriteBytes(id int, data any) (any, error) {
	nc, err := s.lookupConn(id, "writeBytes")
	if err != nil {
		return nil, err
	}
	arr, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("writeBytes: data is not a byte buffer")
	}
	buf := make([]byte, len(arr))
	for i, v := range arr {
		n := toInt(v)
		if n < 0 || n > 255 {
			return nil, fmt.Errorf("writeBytes: byte value %d out of range (must be 0-255)", n)
		}
		buf[i] = byte(n)
	}
	if nc.c == nil {
		return false, nil
	}
	r := s.netConns
	if err := nc.c.SetWriteDeadline(r.deadline(nc)); err != nil {
		r.fail(nc, fmt.Sprintf("writeBytes: %v", err))
		return false, nil
	}
	written := 0
	for written < len(buf) {
		n, err := nc.c.Write(buf[written:])
		if err != nil {
			r.fail(nc, describeNetErr("writeBytes", nc, err))
			return false, nil
		}
		written += n
	}
	return true, nil
}

// ioCloseConn implements the `closeConn(c: int) -> bool` builtin: closes c
// and drops it from the registry. Closing an already-closed (or unknown)
// handle is a clean error, not a panic, matching every other builtin's
// bad-input contract in this codebase.
func (s *Server) ioCloseConn(id int) (any, error) {
	r := s.netConns
	r.mu.Lock()
	nc, ok := r.conns[id]
	if ok {
		delete(r.conns, id)
	}
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("closeConn: connection %d does not exist (already closed?)", id)
	}
	if nc.c == nil {
		return true, nil
	}
	if err := nc.c.Close(); err != nil {
		return nil, fmt.Errorf("closeConn: %v", err)
	}
	return true, nil
}

func (r *netRegistry) markEOF(nc *netConn) {
	r.mu.Lock()
	nc.eof = true
	r.mu.Unlock()
}

// ioPollBytes implements `pollBytes(c: int, maxLen: int, waitMs: int) ->
// [int]`: readBytes for a reader that has other work — up to maxLen bytes
// that arrive within waitMs (0: only what is already waiting, give or take
// the millisecond a socket read needs), else []. Unlike readBytes, nothing
// arriving in time is not a failure: connError stays "" and the connection
// stays usable, which is what lets one proc interleave a long-lived stream
// (an SSE subscription) with its own requests, draining the stream between
// them. connOpen says whether [] means "not yet" or "never again".
func (s *Server) ioPollBytes(id int, maxLen int, waitMs int) (any, error) {
	if maxLen <= 0 {
		return nil, fmt.Errorf("pollBytes: maxLen must be positive, got %d", maxLen)
	}
	if waitMs < 0 || waitMs > maxConnTimeoutMs {
		return nil, fmt.Errorf("pollBytes: waitMs %d is out of range (must be 0-%d)", waitMs, maxConnTimeoutMs)
	}
	nc, err := s.lookupConn(id, "pollBytes")
	if err != nil {
		return nil, err
	}
	r := s.netConns
	r.mu.Lock()
	ended := nc.c == nil || nc.eof || nc.err != ""
	r.mu.Unlock()
	if ended {
		return []any{}, nil
	}
	wait := time.Duration(waitMs) * time.Millisecond
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	if err := nc.c.SetReadDeadline(time.Now().Add(wait)); err != nil {
		r.fail(nc, fmt.Sprintf("pollBytes: %v", err))
		return []any{}, nil
	}
	buf := make([]byte, maxLen)
	n, err := nc.c.Read(buf)
	if err != nil && n == 0 {
		var ne net.Error
		switch {
		case errors.As(err, &ne) && ne.Timeout():
			// Nothing yet: not a failure.
		case errors.Is(err, io.EOF):
			r.markEOF(nc)
		default:
			r.fail(nc, fmt.Sprintf("pollBytes: %v", err))
		}
		return []any{}, nil
	}
	out := make([]any, n)
	for i := 0; i < n; i++ {
		out[i] = int(buf[i])
	}
	return out, nil
}

// ioConnOpen implements `connOpen(c: int) -> bool`: false once c can never
// deliver another byte — its dial failed, the peer closed it, or a
// transport failure (connError) ended it.
func (s *Server) ioConnOpen(id int) (any, error) {
	nc, err := s.lookupConn(id, "connOpen")
	if err != nil {
		return nil, err
	}
	r := s.netConns
	r.mu.Lock()
	defer r.mu.Unlock()
	return nc.c != nil && !nc.eof && nc.err == "", nil
}

// ioShutdownConn implements `shutdownConn(c: int) -> bool`: ends c's read
// side — a readBytes blocked on c, in any goroutine, returns [] (a clean
// EOF) and so does every later one — while the handle stays registered, so
// whichever task owns c still closes it with closeConn. It is how one task
// stops another that is parked in readBytes without pulling the handle out
// from under it: closeConn would, and the reader's next call on the handle
// would then be an unknown-handle abort instead of an ordinary end. A
// connection whose dial failed has nothing to shut, and answers false.
func (s *Server) ioShutdownConn(id int) (any, error) {
	nc, err := s.lookupConn(id, "shutdownConn")
	if err != nil {
		return nil, err
	}
	if nc.c == nil {
		return false, nil
	}
	// Every handle is TCP (listen/accept/connect only ever mint TCP), and a
	// TCP half-close is persistent: unlike an expired deadline, which the
	// reader's next readBytes would reset, every later read is EOF too.
	tc, ok := nc.c.(*net.TCPConn)
	if !ok {
		return false, nil
	}
	if err := tc.CloseRead(); err != nil {
		s.netConns.fail(nc, fmt.Sprintf("shutdownConn: %v", err))
		return false, nil
	}
	return true, nil
}

// lookupConn resolves a connection handle for readBytes/writeBytes, or a
// clean error naming what's actually wrong (an unknown handle, or one that
// names a listener rather than a connection — an easy mistake to make since
// both are just ints).
func (s *Server) lookupConn(id int, who string) (*netConn, error) {
	r := s.netConns
	r.mu.Lock()
	nc, ok := r.conns[id]
	_, isListener := r.listeners[id]
	r.mu.Unlock()
	if !ok {
		if isListener {
			return nil, fmt.Errorf("%s: %d is a listener handle, not a connection — call accept(l) first", who, id)
		}
		return nil, fmt.Errorf("%s: connection %d does not exist", who, id)
	}
	return nc, nil
}

// ioConnPeer implements `connPeer(c: int) -> text`: the remote address of
// connection c as host:port ("" for a dial that never connected) — what an
// operator log names a client by.
func (s *Server) ioConnPeer(id int) (any, error) {
	nc, err := s.lookupConn(id, "connPeer")
	if err != nil {
		return nil, err
	}
	if nc.c == nil {
		return "", nil
	}
	return nc.c.RemoteAddr().String(), nil
}
