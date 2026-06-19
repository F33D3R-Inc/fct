package runtime

// Billing — a subscription and usage ledger, with a signed provider webhook to
// keep it in sync. Turn it on with FACET_BILLING=1. The runtime then manages two
// reserved tables (subscriptions and a usage meter) and a few reserved actions,
// and exposes a webhook the payment provider posts state changes to.
//
// Facet does not embed a payment SDK — that would couple the single static
// binary to one vendor and one network. Instead it keeps an authoritative local
// ledger: the app records intent (`subscribe`, `recordUsage`), and the provider
// (Stripe, Paddle, a billing service) confirms reality by POSTing to
// /billing/webhook, signed with FACET_BILLING_WEBHOOK_SECRET. That HMAC is the
// trust boundary — an unsigned or mis-signed webhook is rejected — and the same
// `FACET_SECRET`-derived keyring that signs cookies verifies it, so there is one
// secret to manage. A `GET /api/_billing` reports the caller's current standing,
// which an app can gate features on.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"facet/internal/ir"
)

const (
	subscriptionEntity = "FacetSubscription"
	usageEntity        = "FacetUsage"

	subActive   = "active"
	subCanceled = "canceled"
	subPastDue  = "past_due"

	maxWebhookBytes = 1 << 20
)

// billingEnabled reports whether the billing ledger is on (FACET_BILLING=1).
func billingEnabled() bool { return envOn("FACET_BILLING") }

// billingEntities are the reserved tables: one subscription row per subscriber
// (keyed by tenant when multi-tenant, else by user), and an append-only usage
// meter that metered-billing reconciles against.
func billingEntities() []ir.Entity {
	return []ir.Entity{
		{Name: subscriptionEntity, Fields: []ir.Field{
			{Name: "id", Type: "int"},
			{Name: "tenant", Type: "int"},
			{Name: "subscriber", Type: "text"},
			{Name: "customer", Type: "text"},
			{Name: "plan", Type: "text"},
			{Name: "status", Type: "text"},
			{Name: "periodEnd", Type: "int"},
			{Name: "updated", Type: "int"},
		}},
		{Name: usageEntity, Fields: []ir.Field{
			{Name: "id", Type: "int"},
			{Name: "tenant", Type: "int"},
			{Name: "subscriber", Type: "text"},
			{Name: "metric", Type: "text"},
			{Name: "quantity", Type: "int"},
			{Name: "at", Type: "int"},
		}},
	}
}

func isBillingAction(name string) bool {
	switch name {
	case "subscribe", "cancelSubscription", "recordUsage":
		return true
	}
	return false
}

// billingKey identifies who a subscription/usage row belongs to: the active
// tenant when multi-tenant, otherwise the signed-in user. Caller holds s.mu.
func (s *Server) billingKey(sid string) (tid int, subscriber string) {
	actor, _ := s.actorOf(sid)
	if multiTenantEnabled() {
		return activeTenant(s.sessions[sid]), actor
	}
	return 0, actor
}

// runBillingAction dispatches a reserved billing action.
func (s *Server) runBillingAction(w http.ResponseWriter, r *http.Request, name string, args []any) {
	if !billingEnabled() {
		http.Error(w, "billing is not enabled (set FACET_BILLING=1)", http.StatusNotImplemented)
		return
	}
	sid := s.session(w, r)
	switch name {
	case "subscribe":
		s.billingSubscribe(w, sid, argStr(args, 0))
	case "cancelSubscription":
		s.billingCancel(w, sid)
	case "recordUsage":
		s.billingRecordUsage(w, sid, argStr(args, 0), toInt(argAny(args, 1)))
	default:
		http.Error(w, "unknown billing action", http.StatusNotFound)
	}
}

// subscriptionFor finds the subscription row for a (tenant, subscriber), or nil
// (caller holds s.mu).
func (s *Server) subscriptionFor(tid int, subscriber string) record {
	for _, r := range s.entities[subscriptionEntity] {
		if m, ok := r.(record); ok && toInt(m["tenant"]) == tid && toStr(m["subscriber"]) == subscriber {
			return m
		}
	}
	return nil
}

func (s *Server) billingSubscribe(w http.ResponseWriter, sid, plan string) {
	if plan == "" {
		http.Error(w, "a plan is required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	if actor == "" || actor == roleGuest {
		s.mu.Unlock()
		http.Error(w, "sign in first", http.StatusForbidden)
		return
	}
	tid, subscriber := s.billingKey(sid)
	now := int(time.Now().Unix())
	sub := s.subscriptionFor(tid, subscriber)
	if sub == nil {
		sub = s.insertReserved(subscriptionEntity, record{
			"tenant": tid, "subscriber": subscriber, "customer": "",
			"plan": plan, "status": subActive, "periodEnd": now + 30*86400, "updated": now,
		})
	} else {
		sub["plan"] = plan
		sub["status"] = subActive
		sub["periodEnd"] = now + 30*86400
		sub["updated"] = now
		s.saveReserved(subscriptionEntity, sub)
	}
	s.mu.Unlock()
	s.recordAudit(actor, "subscribe", true, plan)
	writeJSON(w, map[string]any{"ok": true, "subscription": sub})
}

func (s *Server) billingCancel(w http.ResponseWriter, sid string) {
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	tid, subscriber := s.billingKey(sid)
	sub := s.subscriptionFor(tid, subscriber)
	if sub == nil {
		s.mu.Unlock()
		http.Error(w, "no active subscription", http.StatusNotFound)
		return
	}
	sub["status"] = subCanceled
	sub["updated"] = int(time.Now().Unix())
	s.saveReserved(subscriptionEntity, sub)
	s.mu.Unlock()
	s.recordAudit(actor, "cancelSubscription", true, "")
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) billingRecordUsage(w http.ResponseWriter, sid, metric string, quantity int) {
	if metric == "" {
		http.Error(w, "a metric is required", http.StatusBadRequest)
		return
	}
	if quantity <= 0 {
		quantity = 1
	}
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	tid, subscriber := s.billingKey(sid)
	s.insertReserved(usageEntity, record{
		"tenant": tid, "subscriber": subscriber, "metric": metric,
		"quantity": quantity, "at": int(time.Now().Unix()),
	})
	s.mu.Unlock()
	s.recordAudit(actor, "recordUsage", true, metric)
	writeJSON(w, map[string]any{"ok": true})
}

// handleBilling reports the caller's billing standing: their subscription (if
// any) and the total metered usage in the current period. An app gates premium
// features on `active` here, or reads it from a mobile/API client.
func (s *Server) handleBilling(w http.ResponseWriter, r *http.Request) {
	if !billingEnabled() {
		http.Error(w, "billing is not enabled", http.StatusNotImplemented)
		return
	}
	sid := s.session(w, r)
	s.mu.Lock()
	tid, subscriber := s.billingKey(sid)
	sub := s.subscriptionFor(tid, subscriber)
	usage := map[string]int{}
	for _, row := range s.entities[usageEntity] {
		if m, ok := row.(record); ok && toInt(m["tenant"]) == tid && toStr(m["subscriber"]) == subscriber {
			usage[toStr(m["metric"])] += toInt(m["quantity"])
		}
	}
	s.mu.Unlock()
	status := "none"
	plan := ""
	if sub != nil {
		status = toStr(sub["status"])
		plan = toStr(sub["plan"])
	}
	writeJSON(w, map[string]any{
		"subscribed": status == subActive,
		"status":     status,
		"plan":       plan,
		"usage":      usage,
	})
}

// handleBillingWebhook accepts a provider's state change, authenticated by an
// HMAC over the raw body in the `X-Facet-Signature` header (hex SHA-256 keyed by
// the billing webhook secret). On a valid signature it upserts the named
// subscription's status/plan/period, so the local ledger tracks the provider.
//
// Body: {"subscriber":"ada","tenant":0,"customer":"cus_123","plan":"pro",
//
//	"status":"active","periodEnd":1750000000}
func (s *Server) handleBillingWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !billingEnabled() {
		http.Error(w, "billing is not enabled", http.StatusNotImplemented)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if !verifyWebhook(body, r.Header.Get("X-Facet-Signature")) {
		http.Error(w, "invalid webhook signature", http.StatusForbidden)
		return
	}
	var ev struct {
		Subscriber string `json:"subscriber"`
		Tenant     int    `json:"tenant"`
		Customer   string `json:"customer"`
		Plan       string `json:"plan"`
		Status     string `json:"status"`
		PeriodEnd  int    `json:"periodEnd"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.Subscriber == "" {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if ev.Status == "" {
		ev.Status = subActive
	}
	s.mu.Lock()
	now := int(time.Now().Unix())
	sub := s.subscriptionFor(ev.Tenant, ev.Subscriber)
	if sub == nil {
		s.insertReserved(subscriptionEntity, record{
			"tenant": ev.Tenant, "subscriber": ev.Subscriber, "customer": ev.Customer,
			"plan": ev.Plan, "status": ev.Status, "periodEnd": ev.PeriodEnd, "updated": now,
		})
	} else {
		if ev.Customer != "" {
			sub["customer"] = ev.Customer
		}
		if ev.Plan != "" {
			sub["plan"] = ev.Plan
		}
		if ev.PeriodEnd != 0 {
			sub["periodEnd"] = ev.PeriodEnd
		}
		sub["status"] = ev.Status
		sub["updated"] = now
		s.saveReserved(subscriptionEntity, sub)
	}
	s.mu.Unlock()
	s.recordAudit("system", "billingWebhook", true, ev.Subscriber+" -> "+ev.Status)
	writeJSON(w, map[string]any{"ok": true})
}

// verifyWebhook checks an HMAC-SHA256 hex signature over the raw payload. The key
// is FACET_BILLING_WEBHOOK_SECRET when set, otherwise a key derived from the
// master secret, so a deployment always has a usable webhook key.
func verifyWebhook(body []byte, presented string) bool {
	if presented == "" {
		return false
	}
	key := []byte(os.Getenv("FACET_BILLING_WEBHOOK_SECRET"))
	if len(key) == 0 {
		key = ring().signKey
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}
