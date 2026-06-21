package compile

import (
	"strings"
	"testing"
)

// A `webhook` is an inbound endpoint: an external system POSTs to a path and the
// runtime runs the named action. It lowers to ir.Webhook with its path, target
// action, and optional secret env var.
const webhookApp = `app Hooks:
    entity Payment:
        id: int
        ref: text
        cents: int
    action record(ref: text, cents: int):
        add Payment { ref: ref, cents: cents }
    webhook "/hooks/pay" -> record secret PAY_KEY
    view Home at "/":
        box:
            text "{count(Payment)}"
`

func TestWebhookLowers(t *testing.T) {
	g, err := String(webhookApp)
	if err != nil {
		t.Fatalf("a webhook targeting a defined action should compile, got: %v", err)
	}
	if len(g.Webhooks) != 1 {
		t.Fatalf("want 1 webhook, got %d", len(g.Webhooks))
	}
	wh := g.Webhooks[0]
	if wh.Path != "/hooks/pay" || wh.Action != "record" || wh.Secret != "PAY_KEY" {
		t.Fatalf("webhook lowered wrong: %+v", wh)
	}
}

func TestWebhookErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"unknown action",
			`app A:
    entity P:
        id: int
    webhook "/h" -> missing
    view H at "/":
        box:
            text "x"`,
			"unknown action",
		},
		{
			"duplicate path",
			`app A:
    entity P:
        id: int
    action a():
        add P {}
    action b():
        add P {}
    webhook "/h" -> a
    webhook "/h" -> b
    view H at "/":
        box:
            text "x"`,
			"redeclared",
		},
		{
			"reserved path",
			`app A:
    entity P:
        id: int
    action a():
        add P {}
    webhook "/api/x" -> a
    view H at "/":
        box:
            text "x"`,
			"reserves",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := String(tc.src)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got: %v", tc.want, err)
			}
		})
	}
}
