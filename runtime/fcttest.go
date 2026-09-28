package runtime

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"facet/internal/ir"
)

// `facet test` is a behavior test runner for Facet apps. A test file is JSON
// describing cases; each case runs against a fresh in-memory app (fully isolated,
// no database), driving real actions through the real runtime — placements,
// policies, and input checks all enforced exactly as in production:
//
//	{
//	  "tests": [
//	    {
//	      "name": "a member can post, and only delete their own",
//	      "as": { "actor": "ada", "role": "member" },
//	      "steps": [
//	        { "run": "post", "args": ["hello"] },
//	        { "expect": "count(Post)", "equals": 1 },
//	        { "as": { "actor": "bob" }, "run": "remove", "args": [1], "fails": "forbidden" }
//	      ]
//	    }
//	  ]
//	}
//
// A step is one of: `run` an action (optionally expecting it to fail with a
// message containing `fails`), `expect` an expression to equal a value, or `seed`
// fixture rows. A step-level `as` sets who runs or evaluates that one step.

type testSuite struct {
	Tests []testCase `json:"tests"`
}

type testCase struct {
	Name  string     `json:"name"`
	As    *testActor `json:"as"`
	Steps []testStep `json:"steps"`
}

type testActor struct {
	Actor    string `json:"actor"`
	Role     string `json:"role"`
	Verified bool   `json:"verified"`
}

type testStep struct {
	// run an action
	Run   string     `json:"run"`
	Args  []any      `json:"args"`
	As    *testActor `json:"as"`
	Fails string     `json:"fails"`
	// Returns asserts the action's reply value (`return expr`); compared like
	// `equals`, so a list or object compares by canonical JSON.
	Returns any `json:"returns"`
	// assert an expression
	Expect string `json:"expect"`
	Equals any    `json:"equals"`
	// seed rows
	Seed map[string][]map[string]any `json:"seed"`
	// deliver a webhook: POST Body (JSON) to the declared `webhook` at this
	// path through the app's real handler, signed with the webhook's key —
	// or with a wrong key (Forge) or none (Unsigned) — carrying IdemKey as
	// the Idempotency-Key when set, and expect the HTTP Status (default 200).
	Webhook  string         `json:"webhook"`
	Body     map[string]any `json:"body"`
	Forge    bool           `json:"forge"`
	Unsigned bool           `json:"unsigned"`
	IdemKey  string         `json:"idemKey"`
	Status   int            `json:"status"`
}

// RunTests runs every case in the suite against a fresh app and writes a TAP-ish
// report to out. It returns the pass and fail counts.
func RunTests(graph *ir.IR, raw []byte, out io.Writer) (int, int, error) {
	var suite testSuite
	if err := json.Unmarshal(raw, &suite); err != nil {
		return 0, 0, fmt.Errorf("test file must be JSON: %w", err)
	}
	if len(suite.Tests) == 0 {
		return 0, 0, fmt.Errorf("no tests found (expected a top-level \"tests\" array)")
	}
	// A test run's servers are the runner's own: their request log would
	// interleave with the report, so it is kept to errors unless asked for.
	if os.Getenv("FACET_LOG_LEVEL") == "" {
		os.Setenv("FACET_LOG_LEVEL", "error")
		defer os.Unsetenv("FACET_LOG_LEVEL")
	}
	pass, fail := 0, 0
	for _, tc := range suite.Tests {
		if err := runCase(graph, tc); err != nil {
			fail++
			fmt.Fprintf(out, "not ok — %s\n    %v\n", tc.Name, err)
		} else {
			pass++
			fmt.Fprintf(out, "ok — %s\n", tc.Name)
		}
	}
	fmt.Fprintf(out, "\n%d passed, %d failed, %d total\n", pass, fail, pass+fail)
	return pass, fail, nil
}

// runCase executes one test case against a fresh in-memory app, returning the
// first failure (or nil if every step held).
func runCase(graph *ir.IR, tc testCase) error {
	srv, err := NewInMemory(graph)
	if err != nil {
		return err
	}
	defer srv.Shutdown()

	actor, role, verified := "tester", "admin", true
	if tc.As != nil {
		actor, role = identityOf(tc.As, actor, role)
		verified = tc.As.Verified
	}

	for i, step := range tc.Steps {
		where := fmt.Sprintf("step %d", i+1)
		switch {
		case step.Seed != nil:
			for ent, rows := range step.Seed {
				for _, row := range rows {
					if _, err := srv.AddRow(ent, row); err != nil {
						return fmt.Errorf("%s: seed %s: %w", where, ent, err)
					}
				}
			}

		case step.Run != "":
			a, r, v := actor, role, verified
			if step.As != nil {
				a, r = identityOf(step.As, a, r)
				v = step.As.Verified
			}
			var err error
			if step.Returns != nil {
				var got any
				got, err = srv.RunValue(a, r, v, step.Run, step.Args)
				if err == nil && !valuesEqual(got, step.Returns) {
					return fmt.Errorf("%s: expected %q to return %s, got %s", where, step.Run, jsonOf(step.Returns), jsonOf(got))
				}
			} else {
				_, err = srv.Run(a, r, v, step.Run, step.Args)
			}
			if step.Fails != "" {
				if err == nil {
					return fmt.Errorf("%s: expected %q to fail with %q, but it succeeded", where, step.Run, step.Fails)
				}
				if !strings.Contains(err.Error(), step.Fails) {
					return fmt.Errorf("%s: expected failure containing %q, got %q", where, step.Fails, err.Error())
				}
			} else if err != nil {
				return fmt.Errorf("%s: action %q failed: %w", where, step.Run, err)
			}

		case step.Expect != "":
			e, err := ir.CompileExpr(graph, step.Expect)
			if err != nil {
				return fmt.Errorf("%s: bad expression %q: %w", where, step.Expect, err)
			}
			// A step's `as` names who evaluates it, for an expectation exactly
			// as for an action: "what does bob see" is `{"as": bob, "expect":
			// …}`, since a visibility rule is only ever a rule about someone.
			a, r, v := actor, role, verified
			if step.As != nil {
				a, r = identityOf(step.As, a, r)
				v = step.As.Verified
			}
			got := srv.EvalExpr(e, a, r, v)
			if !valuesEqual(got, step.Equals) {
				return fmt.Errorf("%s: expected %q == %s, got %s", where, step.Expect, jsonOf(step.Equals), jsonOf(got))
			}

		case step.Webhook != "":
			if err := deliverWebhook(graph, srv, step); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}

		default:
			return fmt.Errorf("%s: a step must be one of run / expect / seed / webhook", where)
		}
	}
	return nil
}

func identityOf(a *testActor, defActor, defRole string) (string, string) {
	actor, role := defActor, defRole
	if a.Actor != "" {
		actor = a.Actor
	}
	if a.Role != "" {
		role = a.Role
	}
	return actor, role
}

// valuesEqual compares an evaluated value with an expected JSON value. Scalars
// compare with the runtime's own coercion-aware equality; lists/objects compare
// by canonical JSON.
func valuesEqual(got, want any) bool {
	switch want.(type) {
	case []any, map[string]any:
		return jsonOf(got) == jsonOf(want)
	default:
		return equal(got, want)
	}
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// deliverWebhook runs a sidecar `webhook` step through the server's own HTTP
// handler, so the signature check, the delivery dedup and the target action
// are exercised exactly as a provider's request would exercise them.
func deliverWebhook(graph *ir.IR, srv *Server, step testStep) error {
	var wh *ir.Webhook
	for i := range graph.Webhooks {
		if graph.Webhooks[i].Path == step.Webhook {
			wh = &graph.Webhooks[i]
		}
	}
	if wh == nil {
		return fmt.Errorf("no webhook is declared at %q", step.Webhook)
	}
	body, err := json.Marshal(step.Body)
	if err != nil {
		return fmt.Errorf("webhook body: %w", err)
	}
	key := webhookKey(wh.Secret)
	if step.Forge {
		key = []byte("not-the-webhook-key")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, step.Webhook, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if !step.Unsigned {
		req.Header.Set("X-Facet-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	if step.IdemKey != "" {
		req.Header.Set("Idempotency-Key", step.IdemKey)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	want := step.Status
	if want == 0 {
		want = http.StatusOK
	}
	if rec.Code != want {
		return fmt.Errorf("webhook %s answered %d %q, want %d", step.Webhook, rec.Code, strings.TrimSpace(rec.Body.String()), want)
	}
	if step.Fails != "" && !strings.Contains(rec.Body.String(), step.Fails) {
		return fmt.Errorf("webhook %s answered %q, want it to contain %q", step.Webhook, strings.TrimSpace(rec.Body.String()), step.Fails)
	}
	return nil
}
