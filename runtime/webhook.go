package runtime

// Inbound webhooks — the counterpart of an outbound `service` call. An external
// system (a payment processor, a transcode worker, any brain) POSTs to a declared
// path; the runtime authenticates it with an HMAC over the raw body, then runs the
// named action with system authority and the JSON body decoded into its params.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"facet/internal/ir"
)

// idemRecord is one webhook delivery's deduplication state: either still in
// flight (done == false) or the finished outcome (status + body) replayed to
// every retry of the same delivery. expires bounds how long the record is kept.
type idemRecord struct {
	done    bool
	status  int
	body    string
	expires time.Time
}

// webhookIdemTTL is how long a processed webhook's outcome is remembered so a
// retry replays it instead of re-running the action. A payment processor retries
// for hours; the default 24h comfortably covers a real retry schedule. Override
// with FACET_WEBHOOK_IDEM_TTL (seconds).
func webhookIdemTTL() time.Duration {
	if v := os.Getenv("FACET_WEBHOOK_IDEM_TTL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 24 * time.Hour
}

// idemBegin claims the dedup key for processing. It returns (rec, replay): when
// replay is true the delivery was already processed (or is being processed) and
// rec carries the outcome to return verbatim — the action must NOT run again.
// When replay is false the caller owns the key and must call idemFinish exactly
// once with the outcome. The whole check-and-claim is one critical section, so
// two concurrent retries can never both run the action (the second sees the
// in-flight marker and is told the delivery is already being handled).
func (s *Server) idemBegin(key string) (*idemRecord, bool) {
	now := time.Now()
	s.idemMu.Lock()
	defer s.idemMu.Unlock()
	// Opportunistic sweep: drop expired records so the map can't grow unbounded.
	for k, r := range s.idem {
		if now.After(r.expires) {
			delete(s.idem, k)
		}
	}
	if r, ok := s.idem[key]; ok {
		return r, true
	}
	rec := &idemRecord{expires: now.Add(webhookIdemTTL())}
	s.idem[key] = rec
	return rec, false
}

// idemFinish records the processed outcome under a claimed key, so every later
// retry of the same delivery replays this exact response without re-running.
func (s *Server) idemFinish(key string, status int, body string) {
	s.idemMu.Lock()
	defer s.idemMu.Unlock()
	if rec, ok := s.idem[key]; ok {
		rec.done = true
		rec.status = status
		rec.body = body
		rec.expires = time.Now().Add(webhookIdemTTL())
	}
}

// idemDrop releases a claimed key without recording an outcome — used when the
// delivery failed to authenticate or decode, so it isn't wrongly remembered as
// processed and a corrected retry can still be handled.
func (s *Server) idemDrop(key string) {
	s.idemMu.Lock()
	delete(s.idem, key)
	s.idemMu.Unlock()
}

// webhookHandler builds the HTTP handler for one declared `webhook`. It accepts a
// POST, verifies an HMAC-SHA256 over the raw body (hex, in the X-Facet-Signature
// header) keyed by the webhook's secret, decodes the JSON body into the target
// action's parameters by name, and runs the action with system identity. A bad or
// missing signature is 403; the action's own status and message flow back on a
// failed run, so a webhook reports validation/permission errors like any caller.
func (s *Server) webhookHandler(wh ir.Webhook) http.HandlerFunc {
	act := s.byAction[wh.Action]
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
		if err != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		sig := r.Header.Get("X-Facet-Signature")
		if !verifyWebhookKey(body, sig, webhookKey(wh.Secret)) {
			http.Error(w, "invalid webhook signature", http.StatusForbidden)
			return
		}
		// Idempotency: a processor retries a delivery on any uncertainty (a timeout,
		// a 5xx, a dropped connection). Without dedup, a retried `confirmPaid` would
		// run twice — a double charge/grant. Key the delivery by its explicit
		// Idempotency-Key header when present, else by its signature (a stable hash of
		// this exact payload), so identical retries collapse onto one processing even
		// when the sender omits the header. The first delivery runs the action; every
		// retry replays its recorded outcome and never touches the store again.
		key := wh.Path + "\x00" + idemKey(r.Header.Get("Idempotency-Key"), sig)
		rec, replay := s.idemBegin(key)
		if replay {
			if !rec.done {
				// A concurrent retry arrived while the first is still running. Tell the
				// caller it is in progress; its own retry will replay the real outcome.
				http.Error(w, "duplicate webhook delivery in progress", http.StatusConflict)
				return
			}
			w.Header().Set("X-Facet-Idempotent-Replay", "1")
			replayOutcome(w, rec.status, rec.body)
			return
		}

		var fields map[string]any
		if len(body) > 0 {
			if err := json.Unmarshal(body, &fields); err != nil {
				s.idemDrop(key) // a malformed body isn't a processed delivery; let a fix retry
				http.Error(w, "bad payload", http.StatusBadRequest)
				return
			}
		}
		// Map the JSON body onto the action's parameters by name, in declared order;
		// a missing field is left nil and coerced to the param's zero in runAction.
		args := make([]any, len(act.Params))
		for i, p := range act.Params {
			args[i] = fields[p.Name]
		}
		_, status, msg := s.runAction(systemSID, act, args)
		if status != http.StatusOK {
			// A rejected delivery (a failed check/policy) is a real, repeatable outcome:
			// remember it so retries replay the same rejection rather than re-running.
			s.idemFinish(key, status, msg)
			http.Error(w, msg, status)
			return
		}
		s.recordAudit("system", act.Name, true, "webhook "+wh.Path)
		body0 := `{"ok":true}`
		s.idemFinish(key, http.StatusOK, body0)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body0))
	}
}

// idemKey picks the deduplication identity for a delivery: the sender's explicit
// Idempotency-Key when it set one, otherwise the payload signature (already a
// per-payload HMAC, so byte-identical retries share it).
func idemKey(header, sig string) string {
	if header != "" {
		return "k:" + header
	}
	return "s:" + sig
}

// replayOutcome re-sends a remembered webhook response: a 2xx replays the stored
// JSON body, a non-2xx replays the recorded error message and status, so a retry
// is indistinguishable from the original processing.
func replayOutcome(w http.ResponseWriter, status int, body string) {
	if status == http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
		return
	}
	http.Error(w, body, status)
}

// webhookKey resolves a webhook's HMAC key: the named env var when set, otherwise
// the master-derived signing key — so a deployment always has a usable key even
// before any secret is configured.
func webhookKey(envName string) []byte {
	if envName != "" {
		if v := os.Getenv(envName); v != "" {
			return []byte(v)
		}
	}
	return ring().signKey
}

// verifyWebhookKey checks an HMAC-SHA256 hex signature over the raw payload using
// the given key, in constant time. An empty presented signature always fails.
func verifyWebhookKey(body []byte, presented string, key []byte) bool {
	if presented == "" {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}
