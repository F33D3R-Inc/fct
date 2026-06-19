package runtime

import (
	"encoding/json"
	"fmt"
	"io"
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
// fixture rows.

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
	// assert an expression
	Expect string `json:"expect"`
	Equals any    `json:"equals"`
	// seed rows
	Seed map[string][]map[string]any `json:"seed"`
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
			_, err := srv.Run(a, r, v, step.Run, step.Args)
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
			got := srv.EvalExpr(e, actor, role, verified)
			if !valuesEqual(got, step.Equals) {
				return fmt.Errorf("%s: expected %q == %s, got %s", where, step.Expect, jsonOf(step.Equals), jsonOf(got))
			}

		default:
			return fmt.Errorf("%s: a step must be one of run / expect / seed", where)
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
