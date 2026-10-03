package sqlx_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
)

// ---------------------------------------------------------------------------
// small in-memory driver fixture
// ---------------------------------------------------------------------------

// memQuery is a canned result set returned by the mem driver for a query.
type memQuery struct {
	columns []string
	rows    [][]driver.Value
}

// memQueries maps query strings to canned result sets.  Tests register the
// queries they need before opening a connection.
var memQueries = map[string]memQuery{}

type memDriver struct{}

func (memDriver) Open(string) (driver.Conn, error) { return &memConn{}, nil }

type memConn struct{}

func (c *memConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("mem: prepare is not supported")
}

func (c *memConn) Close() error { return nil }

func (c *memConn) Begin() (driver.Tx, error) {
	return nil, errors.New("mem: begin is not supported")
}

// QueryerContext lets the driver return canned rows for a query without
// touching the network or a real database.
func (c *memConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	q, ok := memQueries[query]
	if !ok {
		return nil, fmt.Errorf("mem: unknown query %q", query)
	}
	return &memRows{columns: q.columns, rows: q.rows}, nil
}

type memRows struct {
	columns []string
	rows    [][]driver.Value
	pos     int
}

func (r *memRows) Columns() []string { return r.columns }

func (r *memRows) Close() error { return nil }

func (r *memRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.pos]
	if len(row) != len(dest) {
		return fmt.Errorf("mem: expected %d destinations, got %d", len(row), len(dest))
	}
	copy(dest, row)
	r.pos++
	return nil
}

func init() {
	sql.Register("sqlx-mem", memDriver{})
}

// openMem opens a sqlx database backed by the in-memory driver.
func openMem(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlx-mem", "")
	if err != nil {
		t.Fatalf("open mem db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// ---------------------------------------------------------------------------
// test structs
// ---------------------------------------------------------------------------

// joinRow is the destination for a join that produces two columns both
// named "id"; they collide on the id field in strict mode and must be mapped
// to the distinct user_id / team_id fields explicitly.
type joinRow struct {
	ID     int    `db:"id"`
	UserID int    `db:"user_id"`
	TeamID int    `db:"team_id"`
	Name   string `db:"name"`
}

// embeddedBase is embedded into embeddedRow to verify that the original
// name lookup priority (shallow fields win over embedded fields) is kept.
type embeddedBase struct {
	ID int `db:"id"`
}

type embeddedRow struct {
	embeddedBase
	ID int `db:"id"` // same mapped name as the embedded field; the shallow field wins
}

// embeddedTagged embeds a struct with an explicit tag, so its fields are
// reached through a path prefix.
type embeddedTagged struct {
	embeddedBase `db:"base"`
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestCompileStrictDuplicateID(t *testing.T) {
	db := openMem(t)

	_, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), []string{"id", "id", "name"})
	if err == nil {
		t.Fatal("expected strict compile to fail on duplicate id columns, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"0", "1", "id"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestCompileExplicitMappingAndScan(t *testing.T) {
	db := openMem(t)

	plan, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), []string{"id", "id", "name"},
		sqlx.WithExplicitColumns(map[int]string{0: "user_id", 1: "team_id"}))
	if err != nil {
		t.Fatalf("compile with explicit mapping: %v", err)
	}

	wantFields := []string{"user_id", "team_id", "name"}
	if got := plan.Fields(); !reflect.DeepEqual(got, wantFields) {
		t.Errorf("fields = %v, want %v", got, wantFields)
	}

	memQueries["join"] = memQuery{
		columns: []string{"id", "id", "name"},
		rows: [][]driver.Value{
			{int64(1), int64(2), "alice"},
			{int64(3), int64(4), "bob"},
		},
	}

	rows, err := db.Queryx("join")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var got []joinRow
	for rows.Next() {
		var j joinRow
		if err := plan.Scan(rows, &j); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, j)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	want := []joinRow{{UserID: 1, TeamID: 2, Name: "alice"}, {UserID: 3, TeamID: 4, Name: "bob"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestCompileExplicitDuplicateTarget(t *testing.T) {
	db := openMem(t)

	// two columns explicitly mapped to the same field must be rejected even
	// though explicit mapping is allowed.
	_, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), []string{"id", "id", "name"},
		sqlx.WithExplicitColumns(map[int]string{0: "user_id", 1: "user_id"}))
	if err == nil {
		t.Fatal("expected duplicate target error, got nil")
	}
	if !strings.Contains(err.Error(), "user_id") {
		t.Errorf("error %q does not mention target field user_id", err)
	}
}

func TestCompileMissingField(t *testing.T) {
	db := openMem(t)

	_, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), []string{"id", "name", "extra"})
	if err == nil {
		t.Fatal("expected missing field error, got nil")
	}
	if !strings.Contains(err.Error(), "extra") {
		t.Errorf("error %q does not mention missing column extra", err)
	}
}

func TestCompileUnsafeDiscardsUnmapped(t *testing.T) {
	db := openMem(t)

	plan, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), []string{"id", "id", "name", "extra"},
		sqlx.WithExplicitColumns(map[int]string{0: "user_id", 1: "team_id"}),
		sqlx.WithUnsafe())
	if err != nil {
		t.Fatalf("compile unsafe: %v", err)
	}
	if got := plan.Fields(); got[3] != "" {
		t.Errorf("unmapped column should be discarded, got field %q", got[3])
	}

	memQueries["unsafe"] = memQuery{
		columns: []string{"id", "id", "name", "extra"},
		rows:    [][]driver.Value{{int64(7), int64(8), "carol", "ignored"}},
	}

	rows, err := db.Queryx("unsafe")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	rows.Next()
	var j joinRow
	if err := plan.Scan(rows, &j); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if j.UserID != 7 || j.TeamID != 8 || j.Name != "carol" {
		t.Errorf("got %+v, want {7 8 carol}", j)
	}
}

func TestScanConversionFailureKeepsOldValues(t *testing.T) {
	db := openMem(t)

	plan, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), []string{"id", "id", "name"},
		sqlx.WithExplicitColumns(map[int]string{0: "user_id", 1: "team_id"}))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// the second column is a string that cannot convert to int; the first
	// column converted fine but must not be committed.
	memQueries["bad"] = memQuery{
		columns: []string{"id", "id", "name"},
		rows:    [][]driver.Value{{int64(1), "not-a-number", "alice"}},
	}

	rows, err := db.Queryx("bad")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	dest := joinRow{UserID: 99, TeamID: 98, Name: "old"}
	rows.Next()
	if err := plan.Scan(rows, &dest); err == nil {
		t.Fatal("expected conversion error, got nil")
	}

	want := joinRow{UserID: 99, TeamID: 98, Name: "old"}
	if !reflect.DeepEqual(dest, want) {
		t.Errorf("dest changed after failed scan: got %+v, want %+v", dest, want)
	}
}

func TestEmbeddedFieldPriority(t *testing.T) {
	db := openMem(t)

	// "id" resolves to the shallow field (embeddedRow.ID), not the embedded
	// embeddedBase.ID, matching the original Mapper lookup priority.
	plan, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(embeddedRow{}), []string{"id"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := plan.Fields(); got[0] != "id" {
		t.Fatalf("field = %q, want id", got[0])
	}

	memQueries["embed"] = memQuery{
		columns: []string{"id"},
		rows:    [][]driver.Value{{int64(42)}},
	}

	rows, err := db.Queryx("embed")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	rows.Next()
	var e embeddedRow
	if err := plan.Scan(rows, &e); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if e.ID != 42 {
		t.Errorf("shallow field = %d, want 42", e.ID)
	}
	if e.embeddedBase.ID != 0 {
		t.Errorf("embedded field was written: got %d, want 0", e.embeddedBase.ID)
	}
}

func TestExplicitMappingIntoEmbeddedPath(t *testing.T) {
	db := openMem(t)

	plan, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(embeddedTagged{}), []string{"id"},
		sqlx.WithExplicitColumn(0, "base.id"))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := plan.Fields(); got[0] != "base.id" {
		t.Fatalf("field = %q, want base.id", got[0])
	}

	memQueries["embed-tag"] = memQuery{
		columns: []string{"id"},
		rows:    [][]driver.Value{{int64(7)}},
	}

	rows, err := db.Queryx("embed-tag")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	rows.Next()
	var e embeddedTagged
	if err := plan.Scan(rows, &e); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if e.embeddedBase.ID != 7 {
		t.Errorf("embedded field = %d, want 7", e.embeddedBase.ID)
	}
}

func TestCompileRejectsNonStruct(t *testing.T) {
	db := openMem(t)

	if _, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(0), []string{"id"}); err == nil {
		t.Fatal("expected error for non-struct type, got nil")
	}
	if _, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), nil); err == nil {
		t.Fatal("expected error for no columns, got nil")
	}
}

func TestScanRejectsMismatchedColumns(t *testing.T) {
	db := openMem(t)

	plan, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), []string{"id", "id", "name"},
		sqlx.WithExplicitColumns(map[int]string{0: "user_id", 1: "team_id"}))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	memQueries["mismatch"] = memQuery{
		columns: []string{"id", "name"},
		rows:    [][]driver.Value{{int64(1), "alice"}},
	}

	rows, err := db.Queryx("mismatch")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	rows.Next()
	var j joinRow
	if err := plan.Scan(rows, &j); err == nil {
		t.Fatal("expected column mismatch error, got nil")
	}
}

// ---------------------------------------------------------------------------
// demo
// ---------------------------------------------------------------------------

// ExampleColumnMapping demonstrates compiling a column mapping before reading
// any rows, resolving a duplicate "id" join with explicit per-column mappings,
// and scanning the result.
func ExampleColumnMapping() {
	db, err := sqlx.Open("sqlx-mem", "")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	memQueries["demo-join"] = memQuery{
		columns: []string{"id", "id", "name"},
		rows: [][]driver.Value{
			{int64(1), int64(2), "alice"},
			{int64(3), int64(4), "bob"},
		},
	}

	columns := []string{"id", "id", "name"}

	// Strict compilation reports the two conflicting id positions.
	_, err = sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), columns)
	fmt.Println("strict compile:", err)

	// Explicit mapping resolves the conflict; the plan is compiled once and
	// reused for every row.
	plan, err := sqlx.CompileColumnMapping(db.Mapper, reflect.TypeOf(joinRow{}), columns,
		sqlx.WithExplicitColumn(0, "user_id"),
		sqlx.WithExplicitColumn(1, "team_id"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("mapping:")
	fmt.Println(plan)

	rows, err := db.Queryx("demo-join")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var j joinRow
		if err := plan.Scan(rows, &j); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("row: id=%d user_id=%d team_id=%d name=%s\n", j.ID, j.UserID, j.TeamID, j.Name)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}

	// Output:
	// strict compile: sqlx: duplicate column mapping: columns 0 ("id") and 1 ("id") both map to struct field "id"
	// mapping:
	// column 0: "id" -> user_id
	// column 1: "id" -> team_id
	// column 2: "name" -> name
	// row: id=0 user_id=1 team_id=2 name=alice
	// row: id=0 user_id=3 team_id=4 name=bob
}
