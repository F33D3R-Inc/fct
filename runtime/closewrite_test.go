package runtime

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"facet/internal/compile"
)

// closeWrite ends a connection's send side only: the peer reads its answer
// to end-of-stream at once, and what the peer still sends afterwards is read.
// Over TLS the close_notify goes first. A peer already gone is false, never
// an error.
func TestCloseWriteHalfCloses(t *testing.T) {
	g, err := compile.String("app C:\n    proc main(args: [text]) -> int:\n        return 0\n")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	pair := func(wrap func(server, client net.Conn) (net.Conn, net.Conn)) (net.Conn, net.Conn) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		dialed := make(chan net.Conn, 1)
		go func() {
			c, _ := net.Dial("tcp", ln.Addr().String())
			dialed <- c
		}()
		s, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		return wrap(s, <-dialed)
	}
	plain := func(s, c net.Conn) (net.Conn, net.Conn) { return s, c }
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	overTLS := func(s, c net.Conn) (net.Conn, net.Conn) {
		ts := tls.Server(s, &tls.Config{Certificates: []tls.Certificate{cert}})
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
		done := make(chan error, 1)
		go func() { done <- tc.Handshake() }()
		if err := ts.Handshake(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return ts, tc
	}

	for name, wrap := range map[string]func(s, c net.Conn) (net.Conn, net.Conn){"tcp": plain, "tls": overTLS} {
		server, client := pair(wrap)
		id := srv.netConns.mint(&netConn{c: server})
		if _, err := srv.ioWriteBytes(id, []any{int64('n'), int64('o')}); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		got, err := srv.ioCloseWrite(id)
		if err != nil || got != true {
			t.Fatalf("%s: closeWrite = %v, %v; want true", name, got, err)
		}
		client.SetReadDeadline(time.Now().Add(2 * time.Second))
		all, err := io.ReadAll(client)
		if err != nil || string(all) != "no" {
			t.Fatalf("%s: the peer read %q, %v — want the answer, then end of stream", name, all, err)
		}
		// The read side is still open: what the peer sends now arrives.
		if _, err := client.Write([]byte("late")); err != nil {
			t.Fatalf("%s: peer write after the half-close: %v", name, err)
		}
		buf, err := srv.ioReadBytes(id, 16)
		if err != nil || string(bytesOfAny(buf)) != "late" {
			t.Fatalf("%s: read after the half-close = %v, %v", name, buf, err)
		}
		client.Close()
		srv.ioCloseConn(id)
	}

	// A peer that is gone: false, no error.
	server, client := pair(plain)
	client.Close()
	server.Close()
	id := srv.netConns.mint(&netConn{c: server})
	if got, err := srv.ioCloseWrite(id); err != nil || got != false {
		t.Fatalf("closeWrite on a closed connection = %v, %v; want false", got, err)
	}
}

func bytesOfAny(v any) []byte {
	b, _ := bytesOf(v)
	return b
}
