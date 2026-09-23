package runtime

// connectTls — the client half of TLS, for the https endpoints the Rust
// fabric reaches through reqwest's rustls-tls (fabric-facetql,
// fabric-protocol, fabric-daemon). Its servers are plain http; TLS on the
// accepting side is listenTls (tlslisten.go).
//
// `connectTls(host: text, port: int, serverName: text, trustFile: text) ->
// int` (io.net) is connect() plus a TLS 1.2+ handshake, finished inside
// the same connect-time bound, with ALPN offering http/1.1 only (what a
// reqwest built without its http2 feature offers). Every later
// readBytes/writeBytes/pollBytes/closeConn works on the handle exactly as
// on a plain one, and the failure model is connect()'s: a refused dial or a
// failed handshake (an untrusted chain, a name mismatch, a peer that does
// not speak TLS) still mints a handle, whose connError names the cause —
// never an abort, so a client reports "unreachable" as data.
//
// Whom it trusts:
//   - serverName is the name the certificate must carry, and the SNI sent;
//     "" means host, as reqwest verifies against the URL's host.
//   - The roots are the system's (x509.SystemCertPool, which on Linux also
//     honors SSL_CERT_FILE/SSL_CERT_DIR). reqwest's rustls-tls bundles the
//     Mozilla set as webpki-roots instead; a distribution's store is derived
//     from the same Mozilla set, and the operating system's is the one an
//     operator can actually manage.
//   - trustFile, when not "", is a PEM file inside the data sandbox whose
//     certificates are trusted in addition — reqwest's
//     ClientBuilder::add_root_certificate. It is how a deployment on a
//     private CA (or a test with a throwaway one) is trusted explicitly,
//     per connection, without switching verification off: there is no way
//     to skip verification at all. A trustFile that cannot be read or holds
//     no certificate is a configuration error and aborts, like a bad port.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

func (s *Server) ioConnectTls(host string, port int, serverName, trustFile string) (any, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("connectTls: invalid port %d (must be 1-65535)", port)
	}
	if host == "" {
		return nil, fmt.Errorf("connectTls: empty host")
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if trustFile != "" {
		full, err := s.resolveDataPath(trustFile)
		if err != nil {
			return nil, fmt.Errorf("connectTls: %w", err)
		}
		pem, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("connectTls: failed to read trust file %s: %v", trustFile, err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("connectTls: trust file %s holds no PEM certificate", trustFile)
		}
	}
	if serverName == "" {
		serverName = host
	}
	nc := &netConn{outbound: true, timeout: connectDefaultTimeout}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(nc.timeout)
	raw, err := net.DialTimeout("tcp", addr, nc.timeout)
	if err != nil {
		nc.err = fmt.Sprintf("connectTls %s: %v", addr, err)
		return s.netConns.mint(nc), nil
	}
	tc := tls.Client(raw, &tls.Config{
		ServerName: serverName,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		nc.err = fmt.Sprintf("connectTls %s: %v", addr, err)
		return s.netConns.mint(nc), nil
	}
	nc.c = tc
	return s.netConns.mint(nc), nil
}
