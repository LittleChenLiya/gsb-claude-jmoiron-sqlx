//go:build go1.8
// +build go1.8

package sqlx

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type batchPerson struct {
	FirstName string "db:\"first_name\""
	LastName  string "db:\"last_name\""
	Email     string "db:\"email\""
}

const batchInsertQuery = "INSERT INTO batch_person (first_name, last_name, email) VALUES (:first_name, :last_name, :email)"

func batchTestPeople() []batchPerson {
	return []batchPerson{
		{FirstName: "Bin", LastName: "Smuth", Email: "bensmith@allblacks.nz"},
		{FirstName: "Jason", LastName: "Moiron", Email: "jmoiron@jmoiron.net"},
		{FirstName: "John", LastName: "Doe", Email: "jdoe@example.com"},
		{FirstName: "Jane", LastName: "Roe", Email: "jroe@example.com"},
		{FirstName: "Max", LastName: "Mustermann", Email: "max@example.de"},
	}
}

func openBatchTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Connect("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("could not open in-memory sqlite: %v", err)
	}
	// a second connection would see a separate in-memory database
	db.SetMaxOpenConns(1)
	return db
}

// 3 placeholders per row, a budget of 7 and 5 rows must produce 2/2/1 row
// batches, with dollar numbering restarting from 1 in every batch.
func TestNamedExecBatchesChunking(t *testing.T) {
	batches, err := bindNamedBatches(DOLLAR, batchInsertQuery, batchTestPeople(), mapper(), 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batches))
	}

	wantQueries := []string{
		"INSERT INTO batch_person (first_name, last_name, email) VALUES ($1, $2, $3), ($4, $5, $6)",
		"INSERT INTO batch_person (first_name, last_name, email) VALUES ($1, $2, $3), ($4, $5, $6)",
		"INSERT INTO batch_person (first_name, last_name, email) VALUES ($1, $2, $3)",
	}
	wantArgs := [][]interface{}{
		{"Bin", "Smuth", "bensmith@allblacks.nz", "Jason", "Moiron", "jmoiron@jmoiron.net"},
		{"John", "Doe", "jdoe@example.com", "Jane", "Roe", "jroe@example.com"},
		{"Max", "Mustermann", "max@example.de"},
	}
	for i := range wantQueries {
		if batches[i].query != wantQueries[i] {
			t.Errorf("batch %d query:\n got: %s\nwant: %s", i, batches[i].query, wantQueries[i])
		}
		if !reflect.DeepEqual(batches[i].args, wantArgs[i]) {
			t.Errorf("batch %d args:\n got: %v\nwant: %v", i, batches[i].args, wantArgs[i])
		}
	}
}

// repeated named parameters inside the VALUES tuple count once per
// occurrence against the budget and appear in the args in template order.
func TestNamedExecBatchesRepeatedNames(t *testing.T) {
	rows := []map[string]interface{}{
		{"a": 1, "b": 2},
		{"a": 3, "b": 4},
		{"a": 5, "b": 6},
		{"a": 7, "b": 8},
		{"a": 9, "b": 10},
	}
	// :b occurs twice per row, so each row costs 3 parameters even though
	// only 2 distinct names are bound:  budget 7 -> 2/2/1 rows per batch
	batches, err := bindNamedBatches(QUESTION, "INSERT INTO t (x, y, z) VALUES (:b, :a, :b)", rows, mapper(), 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batches))
	}
	wantArgs := [][]interface{}{
		{2, 1, 2, 4, 3, 4},
		{6, 5, 6, 8, 7, 8},
		{10, 9, 10},
	}
	for i := range wantArgs {
		if !reflect.DeepEqual(batches[i].args, wantArgs[i]) {
			t.Errorf("batch %d args:\n got: %v\nwant: %v", i, batches[i].args, wantArgs[i])
		}
	}
}

// named parameters outside the VALUES tuple are fixed: they count once per
// batch against the budget and are bound from the first row of each batch.
func TestNamedExecBatchesFixedParams(t *testing.T) {
	rows := make([]map[string]interface{}, 0, 7)
	for i := 1; i <= 7; i++ {
		rows = append(rows, map[string]interface{}{"id": i, "a": i * 10})
	}
	query := "INSERT INTO kv (id, a) VALUES (:id, :a) ON CONFLICT(id) DO UPDATE SET a = :a"

	// 2 params per row + 1 fixed tail param, budget 7 -> 3/3/1 rows per batch
	batches, err := bindNamedBatches(DOLLAR, query, rows, mapper(), 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batches))
	}
	wantQueries := []string{
		"INSERT INTO kv (id, a) VALUES ($1, $2), ($3, $4), ($5, $6) ON CONFLICT(id) DO UPDATE SET a = $7",
		"INSERT INTO kv (id, a) VALUES ($1, $2), ($3, $4), ($5, $6) ON CONFLICT(id) DO UPDATE SET a = $7",
		"INSERT INTO kv (id, a) VALUES ($1, $2) ON CONFLICT(id) DO UPDATE SET a = $3",
	}
	wantArgs := [][]interface{}{
		{1, 10, 2, 20, 3, 30, 10},
		{4, 40, 5, 50, 6, 60, 40},
		{7, 70, 70},
	}
	for i := range wantQueries {
		if batches[i].query != wantQueries[i] {
			t.Errorf("batch %d query:\n got: %s\nwant: %s", i, batches[i].query, wantQueries[i])
		}
		if !reflect.DeepEqual(batches[i].args, wantArgs[i]) {
			t.Errorf("batch %d args:\n got: %v\nwant: %v", i, batches[i].args, wantArgs[i])
		}
	}

	// the generated statements must be valid SQL
	db := openBatchTestDB(t)
	defer db.Close()
	db.MustExec("CREATE TABLE kv (id integer primary key, a integer)")
	results, err := NamedExecBatchesContext(context.Background(), db, query, rows, 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM kv"); err != nil {
		t.Fatal(err)
	}
	if count != 7 {
		t.Errorf("expected 7 rows, got %d", count)
	}
}

// the whole batch runs inside the caller's transaction and every inserted
// value can be read back from in-memory sqlite.
func TestNamedExecBatchesContext(t *testing.T) {
	db := openBatchTestDB(t)
	defer db.Close()
	db.MustExec("CREATE TABLE batch_person (first_name text, last_name text, email text)")

	people := batchTestPeople()
	ctx := context.Background()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := NamedExecBatchesContext(ctx, tx, batchInsertQuery, people, 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 batch results, got %d", len(results))
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

	var got []batchPerson
	if err := db.Select(&got, "SELECT first_name, last_name, email FROM batch_person ORDER BY email"); err != nil {
		t.Fatal(err)
	}
	want := []batchPerson{
		{FirstName: "Bin", LastName: "Smuth", Email: "bensmith@allblacks.nz"},
		{FirstName: "John", LastName: "Doe", Email: "jdoe@example.com"},
		{FirstName: "Jason", LastName: "Moiron", Email: "jmoiron@jmoiron.net"},
		{FirstName: "Jane", LastName: "Roe", Email: "jroe@example.com"},
		{FirstName: "Max", LastName: "Mustermann", Email: "max@example.de"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

// a failure in the second batch returns the first batch's result alongside
// the error; rolling back the caller's transaction undoes the first batch.
func TestNamedExecBatchesContextSecondBatchFails(t *testing.T) {
	db := openBatchTestDB(t)
	defer db.Close()
	db.MustExec("CREATE TABLE batch_person (first_name text, last_name text, email text unique)")

	people := batchTestPeople()
	// row 3 is the first row of the second batch and duplicates row 1's email
	people[2].Email = people[0].Email

	ctx := context.Background()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := NamedExecBatchesContext(ctx, tx, batchInsertQuery, people, 7)
	if err == nil {
		t.Fatal("expected an error from the second batch")
	}
	if len(results) != 1 {
		t.Errorf("expected 1 successful batch before the failure, got %d", len(results))
	}
	tx.Rollback()

	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM batch_person"); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows after rollback, got %d", count)
	}
}

// a missing field on the last row must fail the whole call before a single
// statement is sent.
func TestNamedExecBatchesContextMissingField(t *testing.T) {
	db := openBatchTestDB(t)
	defer db.Close()
	db.MustExec("CREATE TABLE batch_person (first_name text, last_name text, email text)")

	rows := []map[string]interface{}{
		{"first_name": "Bin", "last_name": "Smuth", "email": "bensmith@allblacks.nz"},
		{"first_name": "Jason", "last_name": "Moiron", "email": "jmoiron@jmoiron.net"},
		{"first_name": "John", "last_name": "Doe", "email": "jdoe@example.com"},
		{"first_name": "Jane", "last_name": "Roe", "email": "jroe@example.com"},
		{"first_name": "Max", "last_name": "Mustermann"}, // missing email
	}

	ctx := context.Background()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := NamedExecBatchesContext(ctx, tx, batchInsertQuery, rows, 7)
	if err == nil {
		t.Fatal("expected a binding error for the missing field")
	}
	if len(results) != 0 {
		t.Errorf("expected no executed batches, got %d", len(results))
	}
	var count int
	if err := tx.Get(&count, "SELECT COUNT(*) FROM batch_person"); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected zero SQL to be executed, found %d rows", count)
	}
	tx.Rollback()
}

// a cancelled context prevents any batch from being executed.
func TestNamedExecBatchesContextCanceled(t *testing.T) {
	db := openBatchTestDB(t)
	defer db.Close()
	db.MustExec("CREATE TABLE batch_person (first_name text, last_name text, email text)")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results, err := NamedExecBatchesContext(ctx, db, batchInsertQuery, batchTestPeople(), 7)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no executed batches, got %d", len(results))
	}
	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM batch_person"); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows, got %d", count)
	}
}

// comments and string literals in the template are preserved verbatim and
// never scanned for named parameters, even when they contain commas,
// brackets, VALUES keywords or :name lookalikes.
func TestNamedExecBatchesContextCommentsAndStrings(t *testing.T) {
	query := "\n" +
		"\t\t-- a comment mentioning VALUES (:fake_one) and :fake_two\n" +
		"\t\tINSERT INTO batch_person (first_name, last_name, email) /* block comment with :fake_three */\n" +
		"\t\tVALUES (:first_name, '(not, a param)', :email) /* trailing :fake_four */\n\t"
	rows := []map[string]interface{}{
		{"first_name": "Bin", "email": "bensmith@allblacks.nz"},
		{"first_name": "Jason", "email": "jmoiron@jmoiron.net"},
		{"first_name": "John", "email": "jdoe@example.com"},
		{"first_name": "Jane", "email": "jroe@example.com"},
		{"first_name": "Max", "email": "max@example.de"},
	}

	// 2 params per row, budget 5 -> 2/2/1 rows per batch
	batches, err := bindNamedBatches(QUESTION, query, rows, mapper(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(batches))
	}
	if !strings.Contains(batches[0].query, "'(not, a param)'") {
		t.Errorf("expected string literal to be preserved, got: %s", batches[0].query)
	}
	if !strings.Contains(batches[0].query, "-- a comment mentioning VALUES (:fake_one) and :fake_two") {
		t.Errorf("expected comment to be preserved, got: %s", batches[0].query)
	}
	wantArgs := [][]interface{}{
		{"Bin", "bensmith@allblacks.nz", "Jason", "jmoiron@jmoiron.net"},
		{"John", "jdoe@example.com", "Jane", "jroe@example.com"},
		{"Max", "max@example.de"},
	}
	for i := range wantArgs {
		if !reflect.DeepEqual(batches[i].args, wantArgs[i]) {
			t.Errorf("batch %d args:\n got: %v\nwant: %v", i, batches[i].args, wantArgs[i])
		}
	}

	db := openBatchTestDB(t)
	defer db.Close()
	db.MustExec("CREATE TABLE batch_person (first_name text, last_name text, email text)")
	if _, err := NamedExecBatchesContext(context.Background(), db, query, rows, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM batch_person"); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Errorf("expected 5 rows, got %d", count)
	}
	var lastName string
	if err := db.Get(&lastName, "SELECT DISTINCT last_name FROM batch_person"); err != nil {
		t.Fatal(err)
	}
	if lastName != "(not, a param)" {
		t.Errorf("expected literal last_name, got %q", lastName)
	}
}
