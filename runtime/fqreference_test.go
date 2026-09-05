package runtime

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"facet/internal/ir"
)

// A relation removed from the schema used to leave its cascade rule behind, and
// migrate said nothing about it. A storefront changed `Order.product` from a
// relation to an `int`, re-migrated, was told only about a stray index — and the
// next `deleteProduct` destroyed a live order, because fk_Order_product was still
// there cascading through a relation the app no longer had.
//
// This pins the whole decision that replaces that silence:
//
//   - the rule is REPORTED, by name, with the command that removes it;
//   - `facet migrate` REFUSES, because an operator asked for the reconcile and
//     can act on the answer;
//   - the rule is NOT dropped — dropping a data-integrity rule the app cannot
//     prove it authored is not a consequence of running an app;
//   - `Init` warns and boots, because refusing to start would break an app that
//     was serving a minute ago over a rule it has no privilege to fix;
//   - another app's rule on another app's kind is left entirely alone, or one
//     FacetQL could never serve two apps.
//
// Without the fix every assertion below fails at the first one: Migrate returns
// nil, having never even listed the references (this app declares no relation,
// so the old code returned before the admin call).
func TestFQMigrateReportsAnUndeclaredReference(t *testing.T) {
	// The storefront's schema AFTER the change: `product` is a plain int, so this
	// app declares no relation at all and wants no reference.
	order := ir.Entity{Name: "Order", Fields: []ir.Field{
		{Name: "id", Type: "int"},
		{Name: "product", Type: "int"},
	}}
	ents := []ir.Entity{order}

	created := 0
	s := fqTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/admin/indexes" && r.Method == http.MethodGet:
			io.WriteString(w, `[]`)
		case r.URL.Path == "/admin/indexes" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/admin/references" && r.Method == http.MethodGet:
			// What FacetQL still holds: the app's own dead rule, and one belonging
			// to a different app sharing the engine.
			io.WriteString(w, `[{"name":"fk_Order_product","kind":"Order","field":"product",`+
				`"parent_kind":"Product","parent_field":"id","on_delete":"cascade"},`+
				`{"name":"fk_Ticket_event","kind":"Ticket","field":"event",`+
				`"parent_kind":"Event","parent_field":"id","on_delete":"cascade"}]`)
		case strings.HasPrefix(r.URL.Path, "/admin/references"):
			created++ // any write to the reference endpoints, including a DELETE
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/nodes/query":
			io.WriteString(w, `{"nodes":[{"address":"Order:0000000000000000001","kind":"Order",`+
				`"data":"{\"id\":1,\"product\":7}"}],"next":""}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	plan, err := s.Migrate(ents, true)
	if !errors.Is(err, errFQUndeclaredReference) {
		t.Fatalf("Migrate error = %v, want one matching errFQUndeclaredReference", err)
	}
	// Named, with the consequence and the fix — the report that was missing.
	for _, want := range []string{"fk_Order_product", "Order", "product", "facetql reference drop fk_Order_product"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Migrate error %q does not mention %q", err, want)
		}
	}
	if !strings.Contains(strings.Join(plan, "\n"), "fk_Order_product") {
		t.Errorf("the plan must carry the stale rule too, got %q", plan)
	}
	// One FacetQL, many apps: a rule over a kind this app does not declare is not
	// this app's to report and certainly not its to refuse over.
	if strings.Contains(err.Error(), "fk_Ticket_event") {
		t.Errorf("another app's reference must not be reported: %v", err)
	}
	// Reported, never removed.
	if created != 0 {
		t.Errorf("Migrate issued %d write(s) to /admin/references; it must declare nothing it did "+
			"not derive and drop nothing at all", created)
	}

	// Init sees the same condition and answers it the other way: a loud notice,
	// once, and the app boots on data that is fully loaded.
	fqUndeclaredOnce = sync.Once{}
	restore := fqCaptureStderr(t)
	rows, initErr := s.Init(ents)
	if _, second := s.Init(ents); second != nil {
		t.Fatalf("second Init: %v", second)
	}
	warned := restore()
	if initErr != nil {
		t.Fatalf("Init must boot past a stale reference, got %v", initErr)
	}
	if len(rows["Order"]) != 1 {
		t.Errorf("Init loaded %d Order row(s), want 1", len(rows["Order"]))
	}
	if !strings.Contains(warned, "fk_Order_product") {
		t.Errorf("the boot notice must name the stale rule:\n%s", warned)
	}
	// Guarded by a process-wide sync.Once, like the admin notice: Init runs on
	// every boot and every dev reload, and a warning on a loop is one nobody reads.
	if n := strings.Count(warned, "Starting anyway"); n != 1 {
		t.Errorf("stderr carried the notice %d time(s), want exactly 1:\n%s", n, warned)
	}
	if !strings.Contains(warned, "facet migrate") {
		t.Errorf("the boot notice must name the command that resolves it:\n%s", warned)
	}
}
