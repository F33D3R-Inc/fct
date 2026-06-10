package fa

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// RedisBroker is a production cross-instance Broker backed by Redis pub/sub,
// implemented over a raw TCP connection speaking the RESP protocol — so it adds
// ZERO external dependencies to the framework. Plug it in with fa.WithBroker to
// make a multi-instance deployment deliver events across instances:
//
//	b, _ := fa.NewRedisBroker("localhost:6379")
//	app := fa.New(manifest, fa.WithBroker(b), fa.WithSigningKey(key))
//
// One connection publishes (PUBLISH), a second runs the subscribe loop
// (SUBSCRIBE) and reconnects with backoff. Messages are length-prefixed RESP bulk
// strings, so signed event payloads (arbitrary bytes, including CRLF) are carried
// safely. Run instances behind sticky load-balancing; Redis handles the rest.
type RedisBroker struct {
	addr     string
	channel  string
	password string

	pubMu sync.Mutex
	pub   net.Conn
	pubBr *bufio.Reader

	subMu  sync.RWMutex
	subs   []func([]byte)
	closed atomic.Bool
}

// RedisOption configures a RedisBroker.
type RedisOption func(*RedisBroker)

// RedisChannel sets the pub/sub channel name (default "fa"). Use distinct
// channels to isolate multiple apps sharing one Redis.
func RedisChannel(name string) RedisOption { return func(b *RedisBroker) { b.channel = name } }

// RedisPassword authenticates with AUTH on each connection.
func RedisPassword(pw string) RedisOption { return func(b *RedisBroker) { b.password = pw } }

// NewRedisBroker connects to Redis at addr ("host:port") and starts the subscribe
// loop. It returns once the initial subscribe connection is established.
func NewRedisBroker(addr string, opts ...RedisOption) (*RedisBroker, error) {
	b := &RedisBroker{addr: addr, channel: "fa"}
	for _, o := range opts {
		o(b)
	}
	conn, err := b.dial()
	if err != nil {
		return nil, fmt.Errorf("fa: redis broker connect: %w", err)
	}
	go b.subscribeLoop(conn)
	return b, nil
}

// Close stops the broker's loops and connections.
func (b *RedisBroker) Close() {
	b.closed.Store(true)
	b.pubMu.Lock()
	if b.pub != nil {
		b.pub.Close()
	}
	b.pubMu.Unlock()
}

func (b *RedisBroker) dial() (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", b.addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if b.password != "" {
		br := bufio.NewReader(conn)
		if err := writeCommand(conn, "AUTH", b.password); err != nil {
			conn.Close()
			return nil, err
		}
		if _, err := readReply(br); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// Publish sends msg to every subscribing instance via PUBLISH.
func (b *RedisBroker) Publish(msg []byte) error {
	b.pubMu.Lock()
	defer b.pubMu.Unlock()
	if b.pub == nil {
		conn, err := b.dial()
		if err != nil {
			return err
		}
		b.pub, b.pubBr = conn, bufio.NewReader(conn)
	}
	if err := writeCommandBytes(b.pub, "PUBLISH", []byte(b.channel), msg); err != nil {
		b.pub.Close()
		b.pub = nil // reconnect next time
		return err
	}
	if _, err := readReply(b.pubBr); err != nil {
		b.pub.Close()
		b.pub = nil
		return err
	}
	return nil
}

// Subscribe registers a handler for messages from any instance.
func (b *RedisBroker) Subscribe(fn func([]byte)) {
	b.subMu.Lock()
	b.subs = append(b.subs, fn)
	b.subMu.Unlock()
}

func (b *RedisBroker) dispatch(msg []byte) {
	b.subMu.RLock()
	fns := b.subs
	b.subMu.RUnlock()
	for _, fn := range fns {
		fn(msg)
	}
}

// subscribeLoop reads pub/sub messages and reconnects with backoff on failure.
func (b *RedisBroker) subscribeLoop(conn net.Conn) {
	backoff := time.Second
	for !b.closed.Load() {
		if conn == nil {
			var err error
			conn, err = b.dial()
			if err != nil {
				time.Sleep(backoff)
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
		}
		br := bufio.NewReader(conn)
		if err := writeCommand(conn, "SUBSCRIBE", b.channel); err != nil {
			conn.Close()
			conn = nil
			continue
		}
		backoff = time.Second
		// Read messages until the connection errors.
		for {
			reply, err := readReply(br)
			if err != nil {
				if !b.closed.Load() {
					slog.Warn("fa: redis broker subscribe lost; reconnecting", "err", err)
				}
				conn.Close()
				conn = nil
				break
			}
			if payload, ok := pubsubPayload(reply); ok {
				b.dispatch(payload)
			}
		}
	}
}

// pubsubPayload extracts the message bytes from a RESP ["message", channel,
// payload] array; ok is false for subscribe confirmations and other frames.
func pubsubPayload(reply any) ([]byte, bool) {
	arr, ok := reply.([]any)
	if !ok || len(arr) != 3 {
		return nil, false
	}
	kind, ok := arr[0].([]byte)
	if !ok || string(kind) != "message" {
		return nil, false
	}
	payload, ok := arr[2].([]byte)
	return payload, ok
}

// ── minimal RESP codec ───────────────────────────────────────────────────────

func writeCommand(w io.Writer, args ...string) error {
	b := make([][]byte, len(args))
	for i, a := range args {
		b[i] = []byte(a)
	}
	return writeCommandBytes(w, "", b...) // first arg empty sentinel unused
}

// writeCommandBytes writes a RESP array command. If name is non-empty it is the
// first element and rest are the remaining arguments (lets the caller pass binary
// args without a string copy); if name is empty, rest is the full arg list.
func writeCommandBytes(w io.Writer, name string, rest ...[]byte) error {
	var args [][]byte
	if name != "" {
		args = append(args, []byte(name))
		args = append(args, rest...)
	} else {
		args = rest
	}
	var buf []byte
	buf = append(buf, '*')
	buf = append(buf, strconv.Itoa(len(args))...)
	buf = append(buf, '\r', '\n')
	for _, a := range args {
		buf = append(buf, '$')
		buf = append(buf, strconv.Itoa(len(a))...)
		buf = append(buf, '\r', '\n')
		buf = append(buf, a...)
		buf = append(buf, '\r', '\n')
	}
	_, err := w.Write(buf)
	return err
}

// readReply reads one RESP value: simple string/error/int as their Go types,
// bulk string as []byte, array as []any (elements recursively decoded).
func readReply(r *bufio.Reader) (any, error) {
	t, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	switch t {
	case '+':
		return line, nil
	case '-':
		return nil, fmt.Errorf("redis: %s", line)
	case ':':
		n, _ := strconv.Atoi(line)
		return n, nil
	case '$':
		n, _ := strconv.Atoi(line)
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		if _, err := r.Discard(2); err != nil { // trailing CRLF
			return nil, err
		}
		return buf, nil
	case '*':
		n, _ := strconv.Atoi(line)
		if n < 0 {
			return nil, nil
		}
		arr := make([]any, n)
		for i := range arr {
			v, err := readReply(r)
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("redis: unknown reply type %q", t)
	}
}

// readLine reads up to and discards a trailing CRLF, returning the line content.
func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	s = s[:len(s)-1]
	if len(s) > 0 && s[len(s)-1] == '\r' {
		s = s[:len(s)-1]
	}
	return s, nil
}
