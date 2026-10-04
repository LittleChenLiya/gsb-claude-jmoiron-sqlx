package sqlx

import (
	"reflect"
	"strings"
	"testing"
)

// joinRow models the result of a join such as
// SELECT users.id, teams.id, users.name FROM users JOIN teams ...
// whose column list is [id, id, name]. It keeps an "id" field so that, by
// default, both id columns resolve to the same destination.
type joinRow struct {
	ID     int64  `db:"id"`
	UserID int64  `db:"user_id"`
	TeamID int64  `db:"team_id"`
	Name   string `db:"name"`
}

func TestCompileStructScanDuplicateColumnsStrict(t *testing.T) {
	var dest joinRow

	// default strict compilation reports BOTH id column positions
	_, err := CompileStructScan(&dest, []string{"id", "id", "name"})
	if err == nil {
		t.Fatal("expected a conflict error for [id, id, name], got nil")
	}
	msg := err.Error()
	for _, want := range []string{"columns 0", "and 1", `"id"`, "destination field"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
	t.Logf("strict conflict error: %v", err)

	// explicit per-ordinal mapping resolves the conflict
	plan, err := CompileStructScan(&dest, []string{"id", "id", "name"},
		WithColumnFields(
			ColumnField{Column: 0, Field: "user_id"},
			ColumnField{Column: 1, Field: "team_id"},
		),
	)
	if err != nil {
		t.Fatalf("explicit mapping failed to compile: %v", err)
	}

	got := plan.Columns()
	wantMappings := []ColumnMapping{
		{Column: 0, Name: "id", Field: "user_id", Traversal: []int{1}},
		{Column: 1, Name: "id", Field: "team_id", Traversal: []int{2}},
		{Column: 2, Name: "name", Field: "name", Traversal: []int{3}},
	}
	if !reflect.DeepEqual(got, wantMappings) {
		t.Fatalf("mapping = %+v, want %+v", got, wantMappings)
	}
}

func TestCompileStructScanExplicitSameTargetTwice(t *testing.T) {
	var dest joinRow
	// two ordinals may not write the same destination field
	_, err := CompileStructScan(&dest, []string{"id", "id", "name"},
		WithColumnFields(
			ColumnField{Column: 0, Field: "user_id"},
			ColumnField{Column: 1, Field: "user_id"},
		),
	)
	if err == nil {
		t.Fatal("expected conflict error for duplicate explicit target, got nil")
	}
	if !strings.Contains(err.Error(), "both map to destination field") {
		t.Fatalf("unexpected error: %v", err)
	}

	// an explicit assignment and an implicit one still cannot collide
	_, err = CompileStructScan(&dest, []string{"user_id", "user_id"},
		WithColumnFields(ColumnField{Column: 0, Field: "user_id"}))
	if err == nil {
		t.Fatal("expected conflict error for explicit/implicit collision, got nil")
	}
}

func TestCompileStructScanBadOptions(t *testing.T) {
	var dest joinRow
	_, err := CompileStructScan(&dest, []string{"id", "name"},
		WithColumnFields(ColumnField{Column: 5, Field: "name"}))
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected out of range error, got %v", err)
	}

	_, err = CompileStructScan(&dest, []string{"id", "name"},
		WithColumnFields(ColumnField{Column: 0, Field: "does_not_exist"}))
	if err == nil || !strings.Contains(err.Error(), "unknown destination field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}

	if _, err := CompileStructScan(dest, []string{"id"}); err == nil {
		t.Fatal("expected pointer error for non-pointer dest")
	}
}

func TestCompileStructScanMissingColumn(t *testing.T) {
	var dest joinRow
	_, err := CompileStructScan(&dest, []string{"user_id", "nope"})
	if err == nil || !strings.Contains(err.Error(), `column 1`) {
		t.Fatalf("expected missing destination error mentioning column 1, got %v", err)
	}
}

func TestStructScanPlanScanOK(t *testing.T) {
	db := memOpen(t)
	defer db.Close()

	plan, err := CompileStructScan((*joinRow)(nil),
		[]string{"id", "id", "name"},
		WithColumnFields(
			ColumnField{Column: 0, Field: "user_id"},
			ColumnField{Column: 1, Field: "team_id"},
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := db.Queryx("mem:id,id,name|1:7:alice")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Fatalf("unexpected end of rows: %v", rows.Err())
	}
	var row joinRow
	if err := plan.Scan(rows, &row); err != nil {
		t.Fatalf("plan scan: %v", err)
	}
	if row.UserID != 1 || row.TeamID != 7 || row.Name != "alice" {
		t.Fatalf("got %+v, want {1 7 alice}", row)
	}
}

func TestStructScanPlanConversionFailureIsAtomic(t *testing.T) {
	db := memOpen(t)
	defer db.Close()

	plan, err := CompileStructScan((*joinRow)(nil),
		[]string{"id", "id", "name"},
		WithColumnFields(
			ColumnField{Column: 0, Field: "user_id"},
			ColumnField{Column: 1, Field: "team_id"},
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	// second column ("id" -> TeamID) is a value that cannot convert to int64
	rows, err := db.Queryx("mem:id,id,name|1:x:alice")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("unexpected end of rows: %v", rows.Err())
	}

	old := joinRow{UserID: 41, TeamID: 42, Name: "old"}
	dest := old
	err = plan.Scan(rows, &dest)
	if err == nil {
		t.Fatal("expected a conversion error, got nil")
	}
	t.Logf("conversion error (expected): %v", err)
	// target keeps its old values: first column must not have leaked either
	if dest != old {
		t.Fatalf("destination was partially updated on failure:\n got %+v\nwant %+v", dest, old)
	}
}

func TestStructScanPlanReuseAcrossRows(t *testing.T) {
	db := memOpen(t)
	defer db.Close()

	plan, err := CompileStructScan((*joinRow)(nil),
		[]string{"id", "id", "name"},
		WithColumnFields(
			ColumnField{Column: 0, Field: "user_id"},
			ColumnField{Column: 1, Field: "team_id"},
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := db.Queryx("mem:id,id,name|1:7:alice|2:8:bob")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []joinRow
	for rows.Next() {
		var row joinRow
		if err := plan.Scan(rows, &row); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := []joinRow{
		{UserID: 1, TeamID: 7, Name: "alice"},
		{UserID: 2, TeamID: 8, Name: "bob"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// embeddedOut shadows the Id field of embeddedEmbeddedBase, mirroring Go's
// normal field lookup rule where the shallower field wins.
type embeddedEmbeddedBase struct {
	ID   int64  `db:"id"`
	Base string `db:"base"`
}

type embeddedMiddle struct {
	embeddedEmbeddedBase
	Name string `db:"name"`
}

type embeddedOut struct {
	ID int64 `db:"id"`
	embeddedMiddle
}

func TestCompileStructScanKeepsEmbeddedPriority(t *testing.T) {
	var dest embeddedOut

	// "id" must resolve to the shallower field embeddedOut.ID (index [0]),
	// not to the deeply embedded base ID (index [1, 0, 0]).
	plan, err := CompileStructScan(&dest, []string{"id", "name", "base"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	byName := map[string][]int{}
	for _, cm := range plan.Columns() {
		byName[cm.Name] = cm.Traversal
	}
	if !reflect.DeepEqual(byName["id"], []int{0}) {
		t.Fatalf("id resolved to %v, want shallow [0]", byName["id"])
	}
	if !reflect.DeepEqual(byName["base"], []int{1, 0, 1}) {
		t.Fatalf("base resolved to %v, want [1 0 1]", byName["base"])
	}
	if !reflect.DeepEqual(byName["name"], []int{1, 1}) {
		t.Fatalf("name resolved to %v, want [1 1]", byName["name"])
	}

	db := memOpen(t)
	defer db.Close()
	rows, err := db.Queryx("mem:id,name,base|9:alice:b0")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("unexpected end of rows: %v", rows.Err())
	}
	var got embeddedOut
	if err := plan.Scan(rows, &got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.ID != 9 {
		t.Fatalf("shallow ID = %d, want 9", got.ID)
	}
	if got.embeddedMiddle.Name != "alice" || got.embeddedMiddle.embeddedEmbeddedBase.Base != "b0" {
		t.Fatalf("embedded fields not scanned: %+v", got)
	}
	if got.embeddedMiddle.embeddedEmbeddedBase.ID != 0 {
		t.Fatalf("deep base ID = %d, want 0 (shadowed, must not be written)", got.embeddedMiddle.embeddedEmbeddedBase.ID)
	}
}

func TestCompileStructScanFromRows(t *testing.T) {
	db := memOpen(t)
	defer db.Close()

	rows, err := db.Queryx("mem:user_id,name|3:carol")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	plan, err := CompileStructScanFrom(rows, (*joinRow)(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("unexpected end of rows: %v", rows.Err())
	}
	var row joinRow
	if err := plan.Scan(rows, &row); err != nil {
		t.Fatal(err)
	}
	if row.UserID != 3 || row.Name != "carol" {
		t.Fatalf("got %+v, want {3 0 carol}", row)
	}
}

type PtrEmbeddedBase struct {
	ID int64 `db:"id"`
}

type ptrEmbeddedOut struct {
	*PtrEmbeddedBase
	Name string `db:"name"`
}

func TestStructScanPlanNilPointerEmbedded(t *testing.T) {
	db := memOpen(t)
	defer db.Close()

	plan, err := CompileStructScan((*ptrEmbeddedOut)(nil), []string{"id", "name"})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := db.Queryx("mem:id,name|5:dave")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("unexpected end of rows: %v", rows.Err())
	}

	dest := &ptrEmbeddedOut{} // embedded pointer is nil
	if err := plan.Scan(rows, dest); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if dest.PtrEmbeddedBase == nil || dest.ID != 5 || dest.Name != "dave" {
		t.Fatalf("got %+v, want embedded id=5 name=dave", dest)
	}
}

func TestStructScanPlanPointerEmbeddedFailureIsAtomic(t *testing.T) {
	db := memOpen(t)
	defer db.Close()

	plan, err := CompileStructScan((*ptrEmbeddedOut)(nil), []string{"id", "name"})
	if err != nil {
		t.Fatal(err)
	}

	// first column (id, in the pointer embed) is fine, second fails
	rows, err := db.Queryx("mem:id,name|5:x")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("unexpected end of rows: %v", rows.Err())
	}

	dest := &ptrEmbeddedOut{PtrEmbeddedBase: &PtrEmbeddedBase{ID: 9}, Name: "old"}
	oldBase := *dest.PtrEmbeddedBase
	if err := plan.Scan(rows, dest); err == nil {
		t.Fatal("expected conversion error, got nil")
	}
	// the pre-existing embedded struct must not have been touched
	if dest.PtrEmbeddedBase == nil || *dest.PtrEmbeddedBase != oldBase {
		t.Fatalf("embedded struct partially updated: %+v, want %+v", dest.PtrEmbeddedBase, oldBase)
	}
	if dest.Name != "old" {
		t.Fatalf("Name = %q, want %q", dest.Name, "old")
	}
}

// TestDefaultStructScanStillOverwrites locks in the original StructScan
// behavior: duplicate columns silently hit the same field. The new strict
// entry point must not change this default.
func TestDefaultStructScanStillOverwrites(t *testing.T) {
	type both struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
	}
	db := memOpen(t)
	defer db.Close()

	rows, err := db.Queryx("mem:id,id,name|1:7:alice")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("unexpected end of rows: %v", rows.Err())
	}
	var got both
	if err := rows.StructScan(&got); err != nil {
		t.Fatalf("StructScan should still tolerate duplicate columns: %v", err)
	}
	// second id silently wins, as before
	if got.ID != 7 || got.Name != "alice" {
		t.Fatalf("original StructScan behavior changed: got %+v", got)
	}
}
