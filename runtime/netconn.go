package runtime

// The io.net.listen capability's five builtins — listen/accept/readBytes/
// writeBytes/closeConn — real inbound TCP, gated separately from io.net's
// outbound httpGet/httpPost (see internal/ir/build.go's builtinCapability
// doc) and restricted to a daemon body specifically (checkDaemonOnlyBuiltins)
// because accept() and readBytes() are genuinely, indefinitely blocking:
// unlike httpGet/httpPost's ioHTTPClient, there is no timeout here at all —
// a daemon's own goroutine is the one place in this runtime that is safe to
// block forever, since nothing ever joins it and it holds no lock (see
// runtime/daemon.go's doc).
//
// A Listener/Conn is, deliberately, just an opaque int handle — the exact
// same move runtime/channel.go's channelRegistry already makes for a channel
// value, for the same reason: an int is already a legal proc/daemon
// parameter, `let` local, array element, and builtin argument, so a handle
// gets all of that for free just by being one, with no new type anywhere in
// the compiler's type system (see internal/ir/build.go's inferProcType,
// "listen"/"accept" cases).
import (
	"fmt"
	"net"
	"sync"
)

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
	listeners map[int]net.Listener
	conns     map[int]net.Conn
}

func newNetRegistry() *netRegistry {
	return &netRegistry{listeners: map[int]net.Listener{}, conns: map[int]net.Conn{}}
}

// ioListen implements the `listen(port: int) -> int` builtin (io.net.listen):
// binds a TCP listener on port across every local interface and mints its
// handle. The listener stays open for the life of the process (a daemon that
// calls listen() typically does so once, before its accept loop) — there is
// no listenClose builtin in this milestone, matching how a daemon itself is
// never stopped short of process exit (see runtime/daemon.go's doc).
func (s *Server) ioListen(port int) (any, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("listen: invalid port %d (must be 1-65535)", port)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen: %v", err)
	}
	r := s.netConns
	r.mu.Lock()
	r.next++
	id := r.next
	r.listeners[id] = ln
	r.mu.Unlock()
	return id, nil
}

// ioAccept implements the `accept(l: int) -> int` builtin: blocks until a
// client connects to l (or l is closed / the process exits), then mints the
// new connection's own handle. The lookup of l is the only part of this call
// under the registry's mutex — the real Accept() call, which can block
// indefinitely, runs after that mutex is released, exactly as this file's
// doc promises.
func (s *Server) ioAccept(id int) (any, error) {
	r := s.netConns
	r.mu.Lock()
	ln, ok := r.listeners[id]
	r.mu.Unlock()
	if !ok {
		if _, isConn := r.conns[id]; isConn {
			return nil, fmt.Errorf("accept: %d is a connection handle, not a listener", id)
		}
		return nil, fmt.Errorf("accept: listener %d does not exist", id)
	}
	conn, err := ln.Accept()
	if err != nil {
		return nil, fmt.Errorf("accept: %v", err)
	}
	r.mu.Lock()
	r.next++
	cid := r.next
	r.conns[cid] = conn
	r.mu.Unlock()
	return cid, nil
}

// ioReadBytes implements the `readBytes(c: int, maxLen: int) -> [int]`
// builtin: blocks until at least one byte is available (or the peer closes
// the connection, or a transport error), reading up to maxLen bytes in one
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
	conn, err := s.lookupConn(id, "readBytes")
	if err != nil {
		return nil, err
	}
	buf := make([]byte, maxLen)
	n, err := conn.Read(buf)
	if err != nil {
		if n == 0 {
			// io.EOF (clean close) or any other read error both end the
			// connection from this daemon's point of view; there is nothing
			// further to read either way, so this is the same empty result a
			// graceful EOF gets, not a distinct error path — a bad connection
			// surfaces on the NEXT writeBytes/readBytes instead, exactly the
			// way a real socket API reports it.
			return []any{}, nil
		}
	}
	out := make([]any, n)
	for i := 0; i < n; i++ {
		out[i] = int(buf[i])
	}
	return out, nil
}

// ioWriteBytes implements the `writeBytes(c: int, data: [int]) -> bool`
// builtin: writes every byte in data to c, looping over net.Conn.Write until
// the whole buffer is sent or a transport error occurs (a single Write call
// is not guaranteed to consume the whole slice — the same reason
// runtime/io.go's file writes use os.WriteFile's own all-or-nothing
// guarantee, which a raw socket write has no equivalent of). Every element
// must already be a valid byte (0-255) — the same range this runtime's other
// byte-buffer producers (bytes(n), readBytes above) already guarantee, and
// the same defensive re-check runtime/io.go's ioWriteFileBytes already makes
// for its own byte-buffer argument.
func (s *Server) ioWriteBytes(id int, data any) (any, error) {
	conn, err := s.lookupConn(id, "writeBytes")
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
	written := 0
	for written < len(buf) {
		n, err := conn.Write(buf[written:])
		if err != nil {
			return nil, fmt.Errorf("writeBytes: %v", err)
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
	conn, ok := r.conns[id]
	if ok {
		delete(r.conns, id)
	}
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("closeConn: connection %d does not exist (already closed?)", id)
	}
	if err := conn.Close(); err != nil {
		return nil, fmt.Errorf("closeConn: %v", err)
	}
	return true, nil
}

// lookupConn resolves a connection handle for readBytes/writeBytes, or a
// clean error naming what's actually wrong (an unknown handle, or one that
// names a listener rather than a connection — an easy mistake to make since
// both are just ints).
func (s *Server) lookupConn(id int, who string) (net.Conn, error) {
	r := s.netConns
	r.mu.Lock()
	conn, ok := r.conns[id]
	r.mu.Unlock()
	if !ok {
		if _, isListener := r.listeners[id]; isListener {
			return nil, fmt.Errorf("%s: %d is a listener handle, not a connection — call accept(l) first", who, id)
		}
		return nil, fmt.Errorf("%s: connection %d does not exist", who, id)
	}
	return conn, nil
}
