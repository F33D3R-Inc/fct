package runtime

// ── undoing an action whose durable write failed ────────────────────────────
//
// An action mutates the in-memory working set as it runs and persists the whole
// batch at the end, in one transaction. When that transaction fails, the store
// rolls itself back — and the working set, which every projection reads, every
// SSE delta is built from and every @unique check consults, is left holding
// writes the database refused.
//
// Reporting the failure to the caller without undoing them would only trade a
// silent lost write for a loud one: the request says 500, the page goes on
// rendering the edit, the uniqueness check goes on seeing a row that is not
// there, and the whole thing evaporates at the next restart. That is the shape
// the two-set bug took, minus the 500 — so the fix is both halves at once, and
// this is the second half.
//
// It is an undo log rather than a snapshot of the working set: an action touches
// a handful of rows out of a table that may hold millions, so what it costs is
// proportional to what it changed. Every entry is recorded BEFORE the mutation
// it undoes, and only the first entry per target is kept, so replaying the log
// restores the state the action started from however many times it wrote.
//
// A nil *undoLog is a working no-op, so a caller that has nothing to roll back
// (the admin row editor, a compliance sweep) passes nil and pays nothing.

// fieldUndo is one in-place edit of a record's field, and what was there before.
type fieldUndo struct {
	row   record
	field string
	val   any
	had   bool
}

// undoLog is what one action changed in the working set, recorded as it changed
// it.
type undoLog struct {
	rows   map[string][]any // entity -> its row slice as the action found it
	nextID map[string]int   // entity -> its id counter as the action found it
	fields []fieldUndo      // in-place record edits, in the order they were made
	sess   map[string]any   // session state key -> its value before the action
	sessOK map[string]bool  // ...and whether the key existed at all
	actor  string           // the session identity before an `establish`
	role   string
}

func newUndoLog(ses *sessionState) *undoLog {
	u := &undoLog{
		rows:   map[string][]any{},
		nextID: map[string]int{},
		sess:   map[string]any{},
		sessOK: map[string]bool{},
	}
	if ses != nil {
		u.actor, u.role = ses.actor, ses.role
	}
	return u
}

// entity records an entity's row slice and id counter before either is replaced.
// Both travel together because `add` moves both and a rollback that restored one
// would hand the next add an id the working set already holds.
func (u *undoLog) entity(s *Server, ent string) {
	if u == nil {
		return
	}
	if _, seen := u.rows[ent]; seen {
		return // the first snapshot is the one the action started from
	}
	u.rows[ent] = s.entities[ent]
	u.nextID[ent] = s.nextID[ent]
}

// field records a record's field before it is written in place. Rows are mutated
// through the shared map, so restoring the entity's slice does not undo this.
func (u *undoLog) field(row record, name string) {
	if u == nil {
		return
	}
	v, had := row[name]
	u.fields = append(u.fields, fieldUndo{row: row, field: name, val: v, had: had})
}

// state records a per-session scalar before an `assign` overwrites it.
func (u *undoLog) state(sess map[string]any, name string) {
	if u == nil {
		return
	}
	if _, seen := u.sessOK[name]; seen {
		return
	}
	v, had := sess[name]
	u.sess[name], u.sessOK[name] = v, had
}

// rollback puts everything this log recorded back the way it was. Field edits
// unwind newest-first so the earliest recorded value is the one that survives;
// slices and counters are restored wholesale, which is exactly the state they
// held at the first mutation.
func (u *undoLog) rollback(s *Server, ses *sessionState, sess map[string]any) {
	if u == nil {
		return
	}
	for i := len(u.fields) - 1; i >= 0; i-- {
		f := u.fields[i]
		if f.had {
			f.row[f.field] = f.val
		} else {
			delete(f.row, f.field)
		}
	}
	for ent, rows := range u.rows {
		s.entities[ent] = rows
		s.nextID[ent] = u.nextID[ent]
	}
	if sess != nil {
		for name, had := range u.sessOK {
			if had {
				sess[name] = u.sess[name]
			} else {
				delete(sess, name)
			}
		}
	}
	if ses != nil {
		ses.actor, ses.role = u.actor, u.role
	}
}
