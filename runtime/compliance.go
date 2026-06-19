package runtime

// Compliance — the obligations a regulated product carries, as runtime services:
//
//   - i18n: message catalogs (FACET_I18N_DIR/<locale>.json) negotiated per
//     request from `?lang` or Accept-Language. The negotiated locale is stamped
//     on the page (Content-Language) and a client fetches its catalog from
//     /api/_i18n to localize, so one app serves many languages.
//   - GDPR data export: GET /api/_export returns every row, across every entity,
//     that names the caller — their right to access, as machine-readable JSON.
//   - GDPR erasure: POST /api/_erase anonymizes the caller's account and nulls
//     the personal fields that reference them — their right to be forgotten —
//     while preserving referential structure (rows are kept, identifiers removed).
//   - Retention: FACET_RETENTION declares max ages per entity
//     (`Entity:field:days,...`); a daily sweep deletes rows past their limit, so
//     data does not outlive its purpose.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"facet/internal/ir"
)

// ── i18n ─────────────────────────────────────────────────────────────────────

// i18nCatalog holds the loaded message catalogs (locale -> key -> message) and
// the default locale. It is read-only after construction.
type i18nCatalog struct {
	defaultLocale string
	locales       map[string]map[string]string
}

// newI18n loads every <locale>.json from FACET_I18N_DIR. A missing or empty
// directory yields an empty catalog (the app simply serves its literal strings),
// so i18n is zero-config until you add a catalog.
func newI18n() *i18nCatalog {
	c := &i18nCatalog{defaultLocale: "en", locales: map[string]map[string]string{}}
	if v := os.Getenv("FACET_DEFAULT_LOCALE"); v != "" {
		c.defaultLocale = v
	}
	dir := os.Getenv("FACET_I18N_DIR")
	if dir == "" {
		return c
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return c
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		msgs := map[string]string{}
		if json.Unmarshal(raw, &msgs) == nil {
			c.locales[strings.TrimSuffix(name, ".json")] = msgs
		}
	}
	return c
}

// localeFor negotiates a request's locale: an explicit `?lang=` wins, then the
// first Accept-Language tag the catalog knows, then the default.
func (c *i18nCatalog) localeFor(r *http.Request) string {
	if l := r.URL.Query().Get("lang"); l != "" && c.has(l) {
		return l
	}
	for _, tag := range parseAcceptLanguage(r.Header.Get("Accept-Language")) {
		if c.has(tag) {
			return tag
		}
		// fall back to the base subtag (en-US -> en).
		if i := strings.IndexByte(tag, '-'); i > 0 && c.has(tag[:i]) {
			return tag[:i]
		}
	}
	return c.defaultLocale
}

func (c *i18nCatalog) has(locale string) bool {
	if locale == c.defaultLocale {
		return true
	}
	_, ok := c.locales[locale]
	return ok
}

// catalog returns the message map for a locale (possibly empty).
func (c *i18nCatalog) catalog(locale string) map[string]string {
	if m, ok := c.locales[locale]; ok {
		return m
	}
	return map[string]string{}
}

// handleI18n serves the negotiated locale's message catalog, so a web or mobile
// client localizes from the same source the server renders against.
func (s *Server) handleI18n(w http.ResponseWriter, r *http.Request) {
	locale := s.i18n.localeFor(r)
	available := []string{s.i18n.defaultLocale}
	for l := range s.i18n.locales {
		if l != s.i18n.defaultLocale {
			available = append(available, l)
		}
	}
	w.Header().Set("Content-Language", locale)
	writeJSON(w, map[string]any{
		"locale":    locale,
		"default":   s.i18n.defaultLocale,
		"available": available,
		"messages":  s.i18n.catalog(locale),
	})
}

// parseAcceptLanguage returns the language tags of an Accept-Language header in
// preference order (q-values are honored only as a coarse sort: tags are already
// listed most-preferred-first by every real client, so we keep that order and
// just drop the ;q= suffixes).
func parseAcceptLanguage(h string) []string {
	if h == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(h, ",") {
		tag := strings.TrimSpace(part)
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = strings.TrimSpace(tag[:i])
		}
		if tag != "" && tag != "*" {
			out = append(out, tag)
		}
	}
	return out
}

// ── GDPR export / erasure ─────────────────────────────────────────────────────

// handleGDPRExport returns every row that names the subject, across every
// entity, as JSON — the right of access. By default the subject is the caller;
// an admin may export anyone's data with `?user=`.
func (s *Server) handleGDPRExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	sid := s.session(w, r)
	subject, ok := s.gdprSubject(sid, r.URL.Query().Get("user"))
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	data := map[string][]any{}
	for _, e := range s.ir.Entities {
		var matched []any
		for _, row := range s.entities[e.Name] {
			m, isRec := row.(record)
			if !isRec {
				continue
			}
			if rowNames(e, m, subject) {
				matched = append(matched, redactRow(e, m))
			}
		}
		if len(matched) > 0 {
			data[e.Name] = matched
		}
	}
	s.mu.Unlock()
	s.recordAudit(subject, "gdprExport", true, "")
	writeJSON(w, map[string]any{"subject": subject, "exportedAt": time.Now().UTC().Format(time.RFC3339), "data": data})
}

// handleGDPRErase anonymizes the subject: their user account is scrubbed
// (username replaced with an opaque tombstone, credentials and contact cleared)
// and every personal text field that equals their identifier, in every entity,
// is nulled. Rows are preserved (so counts and relations stay intact); only the
// identifying values are removed — the right to erasure without breaking the
// graph. By default the subject is the caller; an admin may erase anyone.
func (s *Server) handleGDPRErase(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.guardMutation(w, r, false) {
		return
	}
	sid := s.session(w, r)
	subject, ok := s.gdprSubject(sid, argUser(r))
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tombstone := "erased-" + randomName()[:12]
	s.mu.Lock()
	erased := 0
	for _, e := range s.ir.Entities {
		for _, row := range s.entities[e.Name] {
			m, isRec := row.(record)
			if !isRec {
				continue
			}
			changed := false
			if e.Name == reservedUserEntity && toStr(m["username"]) == subject {
				m["username"] = tombstone
				m["password"] = ""
				m["email"] = ""
				m["verifyToken"] = ""
				m["resetToken"] = ""
				m["mfaSecret"] = ""
				m["mfaEnabled"] = false
				changed = true
			} else {
				for _, f := range e.Fields {
					if f.Type == "text" && !f.IsRelation() && toStr(m[f.Name]) == subject {
						m[f.Name] = tombstone
						changed = true
					}
				}
			}
			if changed {
				s.persist(s.store.Save(e.Name, m))
				erased++
			}
		}
	}
	// Drop any live sessions belonging to the subject so the erased identity
	// cannot keep acting.
	for id, ses := range s.sessions {
		if ses.actor == subject {
			ses.actor, ses.role, ses.verified = roleGuest, roleGuest, false
			_ = id
		}
	}
	s.mu.Unlock()
	s.recordAudit(subject, "gdprErase", true, itoa(erased)+" rows")
	writeJSON(w, map[string]any{"ok": true, "subject": subject, "rowsAffected": erased})
}

// gdprSubject resolves whose data a request operates on: the caller acts on
// themselves; an admin may name any `user`. Returns ("", false) when the caller
// is unauthenticated or asks for someone else without admin rights.
func (s *Server) gdprSubject(sid, requested string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses := s.sessions[sid]
	if ses == nil || ses.actor == "" || ses.actor == roleGuest {
		return "", false
	}
	if requested == "" || requested == ses.actor {
		return ses.actor, true
	}
	if ses.role == roleAdmin {
		return requested, true
	}
	return "", false
}

// rowNames reports whether a row identifies the subject through any non-relation
// text field (author, owner, username, …).
func rowNames(e ir.Entity, m record, subject string) bool {
	if e.Name == reservedUserEntity {
		return toStr(m["username"]) == subject
	}
	for _, f := range e.Fields {
		if f.Type == "text" && !f.IsRelation() && toStr(m[f.Name]) == subject {
			return true
		}
	}
	return false
}

// redactRow copies a row for export, dropping fields that are secret or
// credential-shaped so an export never leaks a hash or an encrypted secret.
func redactRow(e ir.Entity, m record) record {
	out := record{}
	for k, v := range m {
		out[k] = v
	}
	if e.Name == reservedUserEntity {
		for _, k := range []string{"password", "verifyToken", "resetToken", "mfaSecret"} {
			delete(out, k)
		}
	}
	for _, f := range e.Fields {
		if f.Secret {
			delete(out, f.Name)
		}
	}
	return out
}

// argUser reads an erasure request's optional target user from the form/query.
func argUser(r *http.Request) string {
	if u := r.URL.Query().Get("user"); u != "" {
		return u
	}
	_ = r.ParseForm()
	return r.FormValue("user")
}

// ── retention ─────────────────────────────────────────────────────────────────

// retentionRule deletes rows of Entity whose Field (a unix-seconds timestamp) is
// older than Days.
type retentionRule struct {
	Entity string
	Field  string
	Days   int
}

// retentionRules parses FACET_RETENTION ("Post:created:90,AuditNote:at:30").
func retentionRules() []retentionRule {
	spec := os.Getenv("FACET_RETENTION")
	if spec == "" {
		return nil
	}
	var rules []retentionRule
	for _, part := range strings.Split(spec, ",") {
		f := strings.Split(strings.TrimSpace(part), ":")
		if len(f) != 3 {
			continue
		}
		days, err := strconv.Atoi(strings.TrimSpace(f[2]))
		if err != nil || days <= 0 {
			continue
		}
		rules = append(rules, retentionRule{Entity: strings.TrimSpace(f[0]), Field: strings.TrimSpace(f[1]), Days: days})
	}
	return rules
}

// startRetention launches the daily retention sweep when FACET_RETENTION is set.
// It runs once at startup and then every 24h, for the process lifetime.
func (s *Server) startRetention() {
	rules := retentionRules()
	if len(rules) == 0 {
		return
	}
	go func() {
		s.sweepRetention(rules)
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for range t.C {
			s.sweepRetention(rules)
		}
	}()
}

// sweepRetention deletes every row past its retention horizon, fanning the
// deletions out to live clients (they are real application rows). It validates
// each rule against the schema, so a typo is logged and skipped rather than
// silently dropping nothing — or everything.
func (s *Server) sweepRetention(rules []retentionRule) {
	cutoffNow := time.Now().Unix()
	for _, rule := range rules {
		ent, ok := s.entityByName(rule.Entity)
		if !ok {
			s.obs.log.Warn("retention: unknown entity", slog.String("entity", rule.Entity))
			continue
		}
		if _, ok := fieldOf(ent, rule.Field); !ok {
			s.obs.log.Warn("retention: unknown field", slog.String("entity", rule.Entity), slog.String("field", rule.Field))
			continue
		}
		horizon := cutoffNow - int64(rule.Days)*86400
		s.mu.Lock()
		var removedIDs []int
		rows := s.entities[rule.Entity]
		kept := rows[:0:0]
		for _, row := range rows {
			if m, ok := row.(record); ok && int64(toInt(m[rule.Field])) < horizon {
				removedIDs = append(removedIDs, toInt(m["id"]))
				continue
			}
			kept = append(kept, row)
		}
		entChanged := map[string]bool{}
		if len(removedIDs) > 0 {
			s.entities[rule.Entity] = kept
			entChanged[rule.Entity] = true
			removedSet := map[int]bool{}
			var ops []durOp
			for _, id := range removedIDs {
				removedSet[id] = true
				ops = append(ops, durOp{kind: "delete", entity: rule.Entity, id: id})
			}
			s.cascadeMem(rule.Entity, removedSet, entChanged)
			s.commit(ops)
		}
		s.mu.Unlock()
		if len(entChanged) > 0 {
			deltas := map[string]any{}
			s.mu.Lock()
			for ent := range entChanged {
				deltas[ent] = s.entities[ent]
			}
			s.mu.Unlock()
			s.broadcast(deltas)
			s.obs.log.Info("retention swept", slog.String("entity", rule.Entity), slog.Int("removed", len(removedIDs)))
		}
	}
}
