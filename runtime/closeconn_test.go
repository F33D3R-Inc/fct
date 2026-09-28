package runtime

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"testing"
	"time"

	"facet/internal/compile"
)

// A close whose goodbye cannot be delivered still closes and does not fail
// its caller: closing a TLS connection whose transport is already gone
// (tls.Conn.Close's close_notify fails) answers false, releases the handle,
// and a second close is the ordinary unknown-handle error.
func TestCloseConnToAGonePeerIsNotAnError(t *testing.T) {
	g, err := compile.String("app C:\n    proc main(args: [text]) -> int:\n        return 0\n")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
			_ = tc.Handshake()
			c.Close()
		}
	}()
	raw, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	server := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
	if err := server.Handshake(); err != nil {
		t.Fatal(err)
	}
	raw.Close() // the transport is gone before the close
	id := srv.netConns.mint(&netConn{c: server})
	got, err := srv.ioCloseConn(id)
	if err != nil || got != false {
		t.Fatalf("closeConn on a gone peer = %v, %v; want false and no error", got, err)
	}
	if _, err := srv.ioCloseConn(id); err == nil {
		t.Fatal("the handle is still registered after the close")
	}
}
