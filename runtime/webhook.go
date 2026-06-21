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

	"facet/internal/ir"
)

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
		if !verifyWebhookKey(body, r.Header.Get("X-Facet-Signature"), webhookKey(wh.Secret)) {
			http.Error(w, "invalid webhook signature", http.StatusForbidden)
			return
		}
		var fields map[string]any
		if len(body) > 0 {
			if err := json.Unmarshal(body, &fields); err != nil {
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
			http.Error(w, msg, status)
			return
		}
		s.recordAudit("system", act.Name, true, "webhook "+wh.Path)
		writeJSON(w, map[string]any{"ok": true})
	}
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
