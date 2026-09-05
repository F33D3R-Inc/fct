package runtime

import "testing"

// The bug this function exists for: a host-qualified listen address used to be
// concatenated onto a literal "http://localhost", so the very first line the
// tool printed was an unreachable link.
func TestBrowseURL(t *testing.T) {
	for _, c := range []struct{ addr, want string }{
		{":7373", "http://localhost:7373"},
		{"127.0.0.1:9311", "http://127.0.0.1:9311"},
		{"0.0.0.0:8080", "http://localhost:8080"},
		{"[::]:8080", "http://localhost:8080"},
		{"[::1]:8080", "http://[::1]:8080"},
		{"example.internal:80", "http://example.internal:80"},
		{"localhost", "http://localhost"},
	} {
		if got := BrowseURL(c.addr); got != c.want {
			t.Errorf("BrowseURL(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}
