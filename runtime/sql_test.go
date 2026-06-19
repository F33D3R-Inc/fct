package runtime

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// The pure SQL layer is the brains of the store; these tests pin it without a
// database (the integration tests in server_test.go exercise it against a real
// Postgres when FACET_DATABASE_URL is set).

var post = ir.Entity{Name: "Post", Fields: []ir.Field{
	{Name: "id", Type: "int"},
	{Name: "author", Type: "text"},
	{Name: "likes", Type: "int", Index: true},
}}

var message = ir.Entity{Name: "Message", Fields: []ir.Field{
	{Name: "id", Type: "int"},
	{Name: "to", Type: "User", Ref: "User", Index: true},
	{Name: "body", Type: "text"},
}}

func TestSQLType(t *testing.T) {
	cases := []struct {
		f    ir.Field
		want string
	}{
		{ir.Field{Type: "int"}, "BIGINT"},
		{ir.Field{Type: "text"}, "TEXT"},
		{ir.Field{Type: "bool"}, "BOOLEAN"},
		{ir.Field{Type: "User", Ref: "User"}, "BIGINT"}, // a relation is a BIGINT foreign key
	}
	for _, c := range cases {
		if got := sqlType(c.f); got != c.want {
			t.Errorf("sqlType(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}

func TestColumns(t *testing.T) {
	got := columns(post)
	want := []string{"id", "author", "likes"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("columns = %v, want %v", got, want)
	}
}

func TestCreateTableSQL(t *testing.T) {
	sql := createTableSQL(post)
	for _, sub := range []string{
		`CREATE TABLE IF NOT EXISTS "facet_Post"`,
		`"id" BIGINT PRIMARY KEY`,
		`"author" TEXT`,
		`"likes" BIGINT`,
	} {
		if !strings.Contains(sql, sub) {
			t.Errorf("create table missing %q in:\n%s", sub, sql)
		}
	}
}

func TestForeignKeyAndIndex(t *testing.T) {
	fk := strings.Join(addFKSQL(message), "\n")
	for _, sub := range []string{
		`ALTER TABLE "facet_Message" ADD CONSTRAINT "fk_Message_to"`,
		`REFERENCES "facet_User"("id") ON DELETE CASCADE`,
	} {
		if !strings.Contains(fk, sub) {
			t.Errorf("FK SQL missing %q in:\n%s", sub, fk)
		}
	}
	idx := strings.Join(createIndexSQL(message), "\n")
	if !strings.Contains(idx, `CREATE INDEX IF NOT EXISTS "idx_Message_to" ON "facet_Message" ("to")`) {
		t.Errorf("index SQL missing the relation index:\n%s", idx)
	}
}

func TestUpsertSQLAndArgs(t *testing.T) {
	sql := upsertSQL(post)
	for _, sub := range []string{
		`INSERT INTO "facet_Post" ("id", "author", "likes")`,
		`VALUES ($1, $2, $3)`,
		`ON CONFLICT (id) DO UPDATE SET`,
		`"author" = EXCLUDED."author"`,
		`"likes" = EXCLUDED."likes"`,
	} {
		if !strings.Contains(sql, sub) {
			t.Errorf("upsert missing %q in:\n%s", sub, sql)
		}
	}
	args := rowArgs(post, record{"id": 1, "author": "ada", "likes": 3})
	if len(args) != 3 || args[0] != int64(1) || args[1] != "ada" || args[2] != int64(3) {
		t.Errorf("rowArgs = %#v, want [1 ada 3]", args)
	}
	// a missing field becomes the column's zero value.
	args = rowArgs(post, record{"id": 2})
	if args[1] != "" || args[2] != int64(0) {
		t.Errorf("rowArgs with missing fields = %#v, want [2 \"\" 0]", args)
	}
}

func TestExprSQL(t *testing.T) {
	// where p.likes > 0
	where := &ir.Expr{Kind: "bin", Op: ">",
		L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "likes"},
		R: &ir.Expr{Kind: "lit", Val: 0, VType: "int"}}
	args := []any{}
	got, err := exprSQL(where, "p", &args)
	if err != nil {
		t.Fatal(err)
	}
	if got != `("likes" > $1)` {
		t.Errorf("exprSQL = %q, want (\"likes\" > $1)", got)
	}
	if len(args) != 1 || args[0] != int64(0) {
		t.Errorf("args = %#v, want [0]", args)
	}

	// == maps to =, != maps to <>, && maps to AND.
	eq := &ir.Expr{Kind: "bin", Op: "&&",
		L: &ir.Expr{Kind: "bin", Op: "==",
			L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "to"},
			R: &ir.Expr{Kind: "lit", Val: 1, VType: "int"}},
		R: &ir.Expr{Kind: "bin", Op: "!=",
			L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "body"},
			R: &ir.Expr{Kind: "lit", Val: "", VType: "text"}}}
	args = []any{}
	got, _ = exprSQL(eq, "p", &args)
	if !strings.Contains(got, `"to" = $1`) || !strings.Contains(got, `"body" <> $2`) || !strings.Contains(got, " AND ") {
		t.Errorf("exprSQL compound = %q", got)
	}

	// a reference it cannot push down is an error, not wrong SQL.
	bad := &ir.Expr{Kind: "ref", Name: "draft"}
	if _, err := exprSQL(bad, "p", &args); err == nil {
		t.Error("exprSQL should reject a non-pushable reference")
	}
}

func TestSelectSQL(t *testing.T) {
	where := &ir.Expr{Kind: "bin", Op: ">",
		L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "likes"},
		R: &ir.Expr{Kind: "lit", Val: 0, VType: "int"}}
	sql, args, err := selectSQL(Query{Entity: "Post", Where: where, ItemVar: "p", Order: "likes", Desc: true, Limit: 2}, post)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{
		`SELECT "id", "author", "likes" FROM "facet_Post"`,
		`WHERE (("likes" > $1))`,
		`ORDER BY "likes" DESC, "id" DESC`,
		`LIMIT 3`, // page size 2 + 1 sentinel row
	} {
		if !strings.Contains(sql, sub) {
			t.Errorf("selectSQL missing %q in:\n%s", sub, sql)
		}
	}
	if len(args) != 1 {
		t.Errorf("args = %#v, want one bound literal", args)
	}

	// no order falls back to id; default page size applies the sentinel.
	sql, _, _ = selectSQL(Query{Entity: "Post", Limit: 0}, post)
	if !strings.Contains(sql, `ORDER BY "id" ASC`) {
		t.Errorf("default order should be id asc:\n%s", sql)
	}

	// a cursor adds a keyset predicate so the next page starts past the last row.
	sql, _, err = selectSQL(Query{Entity: "Post", Order: "likes", Desc: true, After: encodeCursor(int64(5), 10)}, post)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, " OR ") || !strings.Contains(sql, `"likes" < $`) {
		t.Errorf("keyset cursor predicate missing:\n%s", sql)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := encodeCursor(int64(42), 7)
	ov, id, ok := decodeCursor(c)
	if !ok || id != 7 || toInt(ov) != 42 {
		t.Errorf("cursor round-trip: ov=%v id=%d ok=%v", ov, id, ok)
	}
	// a text order value survives too.
	c = encodeCursor("ada", 3)
	ov, id, ok = decodeCursor(c)
	if !ok || id != 3 || ov != "ada" {
		t.Errorf("text cursor round-trip: ov=%v id=%d ok=%v", ov, id, ok)
	}
	if _, _, ok := decodeCursor("!!!not-base64!!!"); ok {
		t.Error("a malformed cursor should not decode")
	}
}

func TestPlanMigration(t *testing.T) {
	// empty database: a full create for the entity.
	plan := planMigration(schema{cols: map[string]map[string]bool{}, indexes: map[string]bool{}}, []ir.Entity{post})
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, `CREATE TABLE IF NOT EXISTS "facet_Post"`) {
		t.Errorf("empty-db plan should create the table:\n%s", joined)
	}
	if !strings.Contains(joined, `CREATE INDEX IF NOT EXISTS "idx_Post_likes"`) {
		t.Errorf("empty-db plan should create the index:\n%s", joined)
	}

	// existing table missing a column: an additive ADD COLUMN, no table create.
	sc := schema{
		cols:    map[string]map[string]bool{"facet_Post": {"id": true, "author": true}},
		indexes: map[string]bool{"idx_Post_likes": true},
	}
	plan = planMigration(sc, []ir.Entity{post})
	joined = strings.Join(plan, "\n")
	if strings.Contains(joined, "CREATE TABLE") {
		t.Errorf("existing table should not be recreated:\n%s", joined)
	}
	if !strings.Contains(joined, `ALTER TABLE "facet_Post" ADD COLUMN IF NOT EXISTS "likes" BIGINT`) {
		t.Errorf("plan should add the missing column:\n%s", joined)
	}

	// fully up-to-date: empty plan.
	sc = schema{
		cols:    map[string]map[string]bool{"facet_Post": {"id": true, "author": true, "likes": true}},
		indexes: map[string]bool{"idx_Post_likes": true},
	}
	if plan := planMigration(sc, []ir.Entity{post}); len(plan) != 0 {
		t.Errorf("up-to-date schema should yield an empty plan, got %v", plan)
	}
}
