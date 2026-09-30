//go:build go1.8
// +build go1.8

package sqlx

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

type batchItem struct {
	ID   int    `db:"id"`
	Name string `db:"name"`
	Val  int    `db:"val"`
}

func openBatchTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Connect("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// pin the in-memory database to a single connection
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func fiveBatchItems() []batchItem {
	return []batchItem{
		{ID: 1, Name: "n0", Val: 10},
		{ID: 2, Name: "n1", Val: 20},
		{ID: 3, Name: "n2", Val: 30},
		{ID: 4, Name: "n3", Val: 40},
		{ID: 5, Name: "n4", Val: 50},
	}
}

// 3 placeholders per row, a budget of 7 and 5 rows must batch 2/2/1, and all
// values must land in the database when run inside the caller's transaction.
func TestNamedExecBatchesContextSqlite(t *testing.T) {
	db := openBatchTestDB(t)
	ctx := context.Background()
	db.MustExec(`CREATE TABLE item (id INTEGER PRIMARY KEY, name TEXT, val INTEGER)`)

	items := fiveBatchItems()
	tx := db.MustBeginTx(ctx, nil)
	results, err := NamedExecBatchesContext(ctx, tx,
		`INSERT INTO item (id, name, val) VALUES (:id, :name, :val)`, items, 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(results))
	}
	wantAffected := []int64{2, 2, 1}
	for i, res := range results {
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatal(err)
		}
		if n != wantAffected[i] {
			t.Errorf("batch %d: expected %d rows affected, got %d", i, wantAffected[i], n)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var got []batchItem
	if err := db.Select(&got, `SELECT id, name, val FROM item ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, items) {
		t.Errorf("expected %+v, got %+v", items, got)
	}
}

// DOLLAR bind parameters must be numbered from 1 in every batch, and the
// arguments must be in the exact placeholder order.
func TestNamedExecBatchesDollarNumbering(t *testing.T) {
	items := fiveBatchItems()
	batches, err := bindNamedBatches(DOLLAR,
		`INSERT INTO item (id, name, val) VALUES (:id, :name, :val)`, items, mapper(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batches))
	}
	wantQueries := []string{
		`INSERT INTO item (id, name, val) VALUES ($1, $2, $3),($4, $5, $6)`,
		`INSERT INTO item (id, name, val) VALUES ($1, $2, $3),($4, $5, $6)`,
		`INSERT INTO item (id, name, val) VALUES ($1, $2, $3)`,
	}
	for i, want := range wantQueries {
		if batches[i].query != want {
			t.Errorf("batch %d query:\nexpected %s\ngot      %s", i, want, batches[i].query)
		}
	}
	// the second batch must carry the arguments of rows 3 and 4, in order
	wantArgs := []interface{}{3, "n2", 30, 4, "n3", 40}
	if !reflect.DeepEqual(batches[1].args, wantArgs) {
		t.Errorf("batch 1 args: expected %v, got %v", wantArgs, batches[1].args)
	}
	wantLast := []interface{}{5, "n4", 50}
	if !reflect.DeepEqual(batches[2].args, wantLast) {
		t.Errorf("batch 2 args: expected %v, got %v", wantLast, batches[2].args)
	}
}

// Repeated names inside the tuple count once per occurrence, fixed parameters
// after the tuple count once per statement, and the argument order must
// follow the textual placeholder order:  tuple rows first, then suffix.
func TestNamedExecBatchesArgOrder(t *testing.T) {
	rows := []map[string]interface{}{
		{"x": 1, "z": 2, "w": 9},
		{"x": 3, "z": 4, "w": 8},
	}
	batches, err := bindNamedBatches(QUESTION,
		`INSERT INTO t (a, b, c) VALUES (:z, :x, :z) ON CONFLICT DO UPDATE SET c = :w`,
		rows, mapper(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(batches))
	}
	wantQuery := `INSERT INTO t (a, b, c) VALUES (?, ?, ?),(?, ?, ?) ON CONFLICT DO UPDATE SET c = ?`
	if batches[0].query != wantQuery {
		t.Errorf("expected query %s, got %s", wantQuery, batches[0].query)
	}
	// :z and :x repeat per row in textual order;  :w is bound once from the
	// first row of the batch
	wantArgs := []interface{}{2, 1, 2, 4, 3, 4, 9}
	if !reflect.DeepEqual(batches[0].args, wantArgs) {
		t.Errorf("expected args %v, got %v", wantArgs, batches[0].args)
	}
}

// Fixed parameters outside the VALUES tuple must count against the budget:
// 1 fixed + 2 per row with a limit of 7 allows 3 rows per batch.
func TestNamedExecBatchesFixedParamBudget(t *testing.T) {
	rows := make([]map[string]interface{}, 0, 7)
	for i := 0; i < 7; i++ {
		rows = append(rows, map[string]interface{}{"a": i, "b": i * 10, "up": i * 100})
	}
	batches, err := bindNamedBatches(QUESTION,
		`INSERT INTO t (a, b) VALUES (:a, :b) ON DUPLICATE KEY UPDATE b = :up`,
		rows, mapper(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batches))
	}
	wantArgs := []int{7, 7, 3}
	for i, want := range wantArgs {
		if len(batches[i].args) != want {
			t.Errorf("batch %d: expected %d args, got %d", i, want, len(batches[i].args))
		}
	}
	// the fixed :up parameter comes from the first row of each batch
	if batches[1].args[len(batches[1].args)-1] != 300 {
		t.Errorf("batch 1: expected fixed arg 300, got %v", batches[1].args[len(batches[1].args)-1])
	}
}

// When the second batch fails, the error is returned after the first batch's
// result, and rolling back the caller's transaction undoes everything.
func TestNamedExecBatchesContextSecondBatchFails(t *testing.T) {
	db := openBatchTestDB(t)
	ctx := context.Background()
	db.MustExec(`CREATE TABLE item (id INTEGER PRIMARY KEY, name TEXT UNIQUE, val INTEGER)`)

	items := fiveBatchItems()
	// row 3 (first row of the second batch) violates the UNIQUE constraint
	items[2].Name = items[0].Name

	tx := db.MustBeginTx(ctx, nil)
	results, err := NamedExecBatchesContext(ctx, tx,
		`INSERT INTO item (id, name, val) VALUES (:id, :name, :val)`, items, 7)
	if err == nil {
		t.Fatal("expected an error from the second batch")
	}
	if len(results) != 1 {
		t.Errorf("expected 1 successful batch before the failure, got %d", len(results))
	}
	tx.Rollback()

	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM item`); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows after rollback, got %d", count)
	}
}

// execSpy records every statement that would be executed.
type execSpy struct {
	*DB
	queries []string
}

func (s *execSpy) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	s.queries = append(s.queries, query)
	return s.DB.ExecContext(ctx, query, args...)
}

// A row that cannot be bound (here: the last one) must produce an error
// before any SQL is sent.
func TestNamedExecBatchesContextBindFailureSendsNoSQL(t *testing.T) {
	db := openBatchTestDB(t)
	db.MustExec(`CREATE TABLE item (id INTEGER PRIMARY KEY, name TEXT, val INTEGER)`)

	rows := []map[string]interface{}{
		{"id": 1, "name": "n0", "val": 10},
		{"id": 2, "name": "n1", "val": 20},
		{"id": 3, "name": "n2", "val": 30},
		{"id": 4, "name": "n3", "val": 40},
		{"id": 5, "name": "n4"}, // missing "val"
	}
	spy := &execSpy{DB: db}
	_, err := NamedExecBatchesContext(context.Background(), spy,
		`INSERT INTO item (id, name, val) VALUES (:id, :name, :val)`, rows, 7)
	if err == nil {
		t.Fatal("expected a binding error")
	}
	if len(spy.queries) != 0 {
		t.Errorf("expected no SQL to be sent, got %d statements: %v", len(spy.queries), spy.queries)
	}
}

// A canceled context must abort the batch before anything is persisted.
func TestNamedExecBatchesContextCanceled(t *testing.T) {
	db := openBatchTestDB(t)
	db.MustExec(`CREATE TABLE item (id INTEGER PRIMARY KEY, name TEXT, val INTEGER)`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results, err := NamedExecBatchesContext(ctx, db,
		`INSERT INTO item (id, name, val) VALUES (:id, :name, :val)`, fiveBatchItems(), 7)
	if err == nil {
		t.Fatal("expected a context error")
	}
	if len(results) != 0 {
		t.Errorf("expected no results, got %d", len(results))
	}
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM item`); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows, got %d", count)
	}
}

// Comments and string literals in the template must not confuse the parser:
// neither fake named parameters nor fake VALUES tuples inside them may be
// picked up, and parentheses inside strings must not close the tuple.
func TestNamedExecBatchesContextCommentsAndStrings(t *testing.T) {
	db := openBatchTestDB(t)
	ctx := context.Background()
	db.MustExec(`CREATE TABLE note (id INTEGER PRIMARY KEY, txt TEXT)`)

	query := "INSERT INTO note (id, txt) -- VALUES (:fake, :fake)\n" +
		"VALUES (:id, 'it''s, VALUES (1,2,3)') /* a (block) comment :fake2 */"
	rows := []map[string]interface{}{{"id": 1}, {"id": 2}, {"id": 3}}

	results, err := NamedExecBatchesContext(ctx, db, query, rows, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(results))
	}

	var got []struct {
		ID  int    `db:"id"`
		Txt string `db:"txt"`
	}
	if err := db.Select(&got, `SELECT id, txt FROM note ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	for i, row := range got {
		if row.ID != i+1 {
			t.Errorf("row %d: expected id %d, got %d", i, i+1, row.ID)
		}
		if row.Txt != "it's, VALUES (1,2,3)" {
			t.Errorf("row %d: unexpected txt %q", i, row.Txt)
		}
	}
}

func TestNamedExecBatchesContextInvalidInput(t *testing.T) {
	db := openBatchTestDB(t)
	ctx := context.Background()

	// not an INSERT ... VALUES template
	if _, err := NamedExecBatchesContext(ctx, db, `SELECT * FROM item WHERE id = :id`,
		[]map[string]interface{}{{"id": 1}}, 10); err == nil {
		t.Error("expected an error for a template without VALUES (...)")
	}
	// empty slice
	if _, err := NamedExecBatchesContext(ctx, db, `INSERT INTO item (id) VALUES (:id)`,
		[]map[string]interface{}{}, 10); err == nil {
		t.Error("expected an error for an empty slice")
	}
	// not a slice
	if _, err := NamedExecBatchesContext(ctx, db, `INSERT INTO item (id) VALUES (:id)`,
		map[string]interface{}{"id": 1}, 10); err == nil {
		t.Error("expected an error for a non-slice argument")
	}
	// budget too small for a single row
	if _, err := NamedExecBatchesContext(ctx, db, `INSERT INTO item (id, val) VALUES (:id, :val)`,
		[]map[string]interface{}{{"id": 1, "val": 2}}, 1); err == nil {
		t.Error("expected an error for a too small maxParams")
	}
}
