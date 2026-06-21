package compile

import (
	"strings"
	"testing"
)

// A `on <action> -> <reaction>` trigger fires the reaction when the source action
// completes. It lowers to ir.Trigger and is the non-cron sibling of a job.
const triggerApp = `app Reacts:
    entity Post:
        id: int
        body: text
    entity Notice:
        id: int
        msg: text
    action post(body: text):
        add Post { body: body }
    action fanout():
        add Notice { msg: "new post" }
    on post -> fanout
    view Home at "/":
        box:
            text "{count(Notice)}"
`

func TestTriggerLowers(t *testing.T) {
	g, err := String(triggerApp)
	if err != nil {
		t.Fatalf("a trigger to a zero-arg server action should compile, got: %v", err)
	}
	if len(g.Triggers) != 1 {
		t.Fatalf("want 1 trigger, got %d", len(g.Triggers))
	}
	if g.Triggers[0].On != "post" || g.Triggers[0].Action != "fanout" {
		t.Fatalf("trigger lowered wrong: %+v", g.Triggers[0])
	}
}

func TestTriggerErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"unknown source",
			`app A:
    entity P:
        id: int
    action a():
        add P {}
    on missing -> a
    view H at "/":
        box:
            text "x"`,
			"unknown action",
		},
		{
			"unknown reaction",
			`app A:
    entity P:
        id: int
    action a():
        add P {}
    on a -> missing
    view H at "/":
        box:
            text "x"`,
			"not a defined action",
		},
		{
			"reaction takes args",
			`app A:
    entity P:
        id: int
    action a():
        add P {}
    action b(x: int):
        add P {}
    on a -> b
    view H at "/":
        box:
            text "x"`,
			"zero-argument",
		},
		{
			"direct cycle",
			`app A:
    entity P:
        id: int
    action a():
        add P {}
    on a -> a
    view H at "/":
        box:
            text "x"`,
			"trigger cycle",
		},
		{
			"indirect cycle",
			`app A:
    entity P:
        id: int
    action a():
        add P {}
    action b():
        add P {}
    on a -> b
    on b -> a
    view H at "/":
        box:
            text "x"`,
			"trigger cycle",
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
