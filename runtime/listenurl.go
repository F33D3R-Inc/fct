package runtime

import "strings"

// BrowseURL turns a listen address into a URL a person can actually open.
//
// The banner used to build this by concatenating a literal `http://localhost`
// with the listen address, which is correct for exactly one shape of address:
// the bare-port default `:7373`. Any address that names a host — and `facet run
// <file> 127.0.0.1:9311` is the documented way to pass one — produced
// `http://localhost127.0.0.1:9311`, a URL that resolves to nothing. The first
// thing the tool prints was a link that did not work.
//
// The three cases it has to get right:
//
//	:7373            no host  → localhost, the address the operator meant
//	127.0.0.1:9311   a host   → use it verbatim
//	0.0.0.0:8080     every    → localhost, because "all interfaces" is not
//	[::]:8080        interface  something a browser can be pointed at
//
// The wildcard hosts are the subtle one: they are valid to *bind* and useless
// to *visit*, so echoing them back would be literally accurate and still
// unusable. Rendering them as localhost prints the address that reaches the
// server the operator just started.
func BrowseURL(addr string) string {
	host := addr
	port := ""

	// Rightmost colon separates the port, which is what makes this work for a
	// bracketed IPv6 host (`[::1]:8080`) as well as `host:port`.
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host, port = addr[:i], addr[i+1:]
	}

	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}

	if port == "" {
		return "http://" + host
	}

	return "http://" + host + ":" + port
}
