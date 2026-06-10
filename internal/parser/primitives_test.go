package parser

import (
	"strings"
	"testing"
)

// Each server-rendered primitive parses with its distinctive block recorded.
func TestServerPrimitivesParse(t *testing.T) {
	feed := parseOne(t, "feed Timeline:\n    what:\n        items: PostList\n    order: created_at\n    looks:\n        <ul>{items}</ul>\n")
	if feed.Kind != "feed" || feed.Order != "created_at" {
		t.Fatalf("feed: kind=%q order=%q", feed.Kind, feed.Order)
	}
	if !feed.ServerRendered() || len(feed.Looks) == 0 {
		t.Fatal("feed must be server-rendered with a looks body")
	}

	stream := parseOne(t, "stream LiveChat:\n    what:\n        msgs: MsgList\n    throttle: 200ms\n    window: 100\n    looks:\n        <div>{msgs}</div>\n")
	if stream.Kind != "stream" || stream.Throttle != "200ms" || stream.Window != "100" {
		t.Fatalf("stream: kind=%q throttle=%q window=%q", stream.Kind, stream.Throttle, stream.Window)
	}

	life := parseOne(t, "lifecycle Checkout:\n    what:\n        step: str\n    states: cart, pay, done\n    looks:\n        <div>{step}</div>\n")
	if life.Kind != "lifecycle" || strings.Join(life.States, ",") != "cart,pay,done" {
		t.Fatalf("lifecycle: kind=%q states=%v", life.Kind, life.States)
	}

	pipe := parseOne(t, "pipe Prices:\n    what:\n        v: float\n    throttle: 1s\n    looks:\n        <span>{v}</span>\n")
	if pipe.Kind != "pipe" || pipe.Throttle != "1s" {
		t.Fatalf("pipe: kind=%q throttle=%q", pipe.Kind, pipe.Throttle)
	}
}

// Client-rendered primitives parse their render block into Client, not Looks.
func TestClientPrimitivesParse(t *testing.T) {
	vault := parseOne(t, "vault DM:\n    what:\n        envelope: str\n    decrypt:\n        <p>{plaintext}</p>\n")
	if vault.Kind != "vault" || vault.ServerRendered() {
		t.Fatalf("vault must be client-rendered, kind=%q", vault.Kind)
	}
	if len(vault.Looks) != 0 || len(vault.Client) == 0 {
		t.Fatalf("vault body must land in Client, not Looks (looks=%d client=%d)", len(vault.Looks), len(vault.Client))
	}

	media := parseOne(t, "media Clip:\n    what:\n        url: str\n    source:\n        <hls src=\"{url}\"/>\n")
	if media.Kind != "media" || media.ServerRendered() || len(media.Client) == 0 {
		t.Fatalf("media: kind=%q client=%d", media.Kind, len(media.Client))
	}

	signal := parseOne(t, "signal Typing:\n    what:\n        who: str\n    ttl: 5s\n")
	if signal.Kind != "signal" || signal.ServerRendered() || signal.TTL != "5s" {
		t.Fatalf("signal: kind=%q ttl=%q", signal.Kind, signal.TTL)
	}
}

// The kind-gating errors must be precise and teaching.
func TestPrimitiveErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"unknown kind", "widget Foo:\n    what:\n        x: int\n", "unknown primitive"},
		{"vault with looks", "vault DM:\n    what:\n        e: str\n    looks:\n        <p>x</p>\n", "renders on the client"},
		{"throttle on facet", "facet Card:\n    what:\n        x: int\n    throttle: 1s\n    looks:\n        <p>{x}</p>\n", "only valid in a stream or pipe"},
		{"decrypt on feed", "feed F:\n    what:\n        x: int\n    decrypt:\n        <p>{x}</p>\n", "only valid in a vault"},
		{"window on pipe", "pipe P:\n    what:\n        x: int\n    window: 5\n    looks:\n        <p>{x}</p>\n", "only valid in a stream"},
		{"states on facet", "facet C:\n    what:\n        x: int\n    states: a, b\n    looks:\n        <p>{x}</p>\n", "only valid in a lifecycle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got: %v", c.want, err)
			}
		})
	}
}

// A plain facet still parses with Kind "facet" — no behavioral change.
func TestFacetKindDefault(t *testing.T) {
	f := parseOne(t, "facet Card:\n    what:\n        x: int\n    looks:\n        <p>{x}</p>\n")
	if f.Kind != "facet" || !f.ServerRendered() {
		t.Fatalf("plain facet: kind=%q serverRendered=%v", f.Kind, f.ServerRendered())
	}
}
