package sqlx

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type gAuthor struct {
	ID     int64
	Name   string
	Region string
	Books  []gBook
}

type gBook struct {
	ID    int64
	Title string
}

// gAuthorComposite uses a composite key (country, code) and a nullable child
// key.
type gAuthorComposite struct {
	Country string
	Code    int64
	Name    string
	Books   []gEdition
}

type gEdition struct {
	Code  int64
	Seq   sql.NullInt64 `db:"seq"`
	Title string
}

var authorBookColumns = []string{
	"author.id", "author.name", "author.region",
	"book.id", "book.title",
}

var authorBookCfg = GroupConfig{
	ParentPrefix: "author",
	ChildPrefix:  "book",
	ParentKeys:   []string{"author.id"},
	ChildKeys:    []string{"book.id"},
}

// orderedRows returns two authors (2, 1), three distinct books (20, 21, 10)
// and several duplicate rows. First-seen order must be preserved.
func orderedRows() [][]driver.Value {
	return [][]driver.Value{
		{int64(2), "brian", "west", int64(20), "meet me"},
		{int64(2), "brian", "west", int64(20), "meet me"}, // identical duplicate
		{int64(2), "brian", "west", int64(21), "southern"},
		{int64(1), "jason", "east", int64(10), "first light"},
		{int64(1), "jason", "east", int64(10), "first light"}, // identical duplicate
	}
}

func queryFixture(t *testing.T, columns []string, rows [][]driver.Value, fixture *memResult) (*DB, *memResult) {
	t.Helper()
	if fixture == nil {
		fixture = &memResult{columns: columns, rows: rows}
	}
	db := newMemDB("fixture-"+t.Name(), fixture)
	return db, fixture
}

func openRows(t *testing.T, db *DB) *Rows {
	t.Helper()
	rows, err := db.Queryx("SELECT author_book_join")
	if err != nil {
		t.Fatalf("Queryx: %v", err)
	}
	return rows
}

func TestScanGroupsOrdersAndDeduplicates(t *testing.T) {
	db, fixture := queryFixture(t, authorBookColumns, orderedRows(), nil)
	rows := openRows(t, db)

	var authors []gAuthor
	if err := rows.ScanGroups(&authors, authorBookCfg); err != nil {
		t.Fatalf("ScanGroups: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := []gAuthor{
		{ID: 2, Name: "brian", Region: "west", Books: []gBook{
			{ID: 20, Title: "meet me"},
			{ID: 21, Title: "southern"},
		}},
		{ID: 1, Name: "jason", Region: "east", Books: []gBook{
			{ID: 10, Title: "first light"},
		}},
	}
	if !reflect.DeepEqual(authors, want) {
		t.Fatalf("unexpected grouping:\n got %#v\nwant %#v", authors, want)
	}
	if got := fixture.rowsCloseCount(); got != 1 {
		t.Fatalf("rows closed %d times, want exactly 1", got)
	}
}

func TestScanGroupsColumnReorder(t *testing.T) {
	base := orderedRows()
	reordered := make([][]driver.Value, len(base))
	perm := []int{4, 2, 0, 3, 1} // title, region, author.id, book.id, name
	columns := make([]string, len(authorBookColumns))
	for i, j := range perm {
		columns[i] = authorBookColumns[j]
	}
	for r, row := range base {
		reordered[r] = make([]driver.Value, len(row))
		for i, j := range perm {
			reordered[r][i] = row[j]
		}
	}

	db, _ := queryFixture(t, columns, reordered, nil)
	rows := openRows(t, db)
	var authors []gAuthor
	if err := rows.ScanGroups(&authors, authorBookCfg); err != nil {
		t.Fatalf("ScanGroups: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if authors[0].ID != 2 || authors[1].ID != 1 {
		t.Fatalf("parent order not preserved after column reorder: %#v", authors)
	}
	if authors[0].Books[0].Title != "meet me" || authors[0].Books[1].Title != "southern" {
		t.Fatalf("book order/content wrong after column reorder: %#v", authors[0].Books)
	}
}

func TestScanGroupsNullChildrenAndCompositeKeys(t *testing.T) {
	columns := []string{"a.country", "a.code", "a.name", "b.code", "b.seq", "b.title"}
	rowsData := [][]driver.Value{
		{"cn", int64(1), "ming", int64(9), int64(1), "hardcover"},
		{"cn", int64(1), "ming", nil, nil, nil},                   // LEFT JOIN row, no child
		{"cn", int64(1), "ming", int64(9), int64(1), "hardcover"}, // identical
		{"cn", int64(0), "zero", nil, nil, nil},                   // parent code zero vs NULL-free
	}
	cfg := GroupConfig{
		ParentPrefix: "a",
		ChildPrefix:  "b",
		ParentKeys:   []string{"a.country", "a.code"},
		ChildKeys:    []string{"b.code", "b.seq"},
	}

	db, _ := queryFixture(t, columns, rowsData, nil)
	rows := openRows(t, db)
	var authors []gAuthorComposite
	if err := rows.ScanGroups(&authors, cfg); err != nil {
		t.Fatalf("ScanGroups: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(authors) != 2 {
		t.Fatalf("want 2 parents, got %d: %#v", len(authors), authors)
	}
	first := authors[0]
	if len(first.Books) != 1 {
		t.Fatalf("all-NULL child rows must not create objects; got %#v", first.Books)
	}
	book := first.Books[0]
	if book.Code != 9 || !book.Seq.Valid || book.Seq.Int64 != 1 || book.Title != "hardcover" {
		t.Fatalf("unexpected child: %#v", book)
	}
	if authors[1].Code != 0 || len(authors[1].Books) != 0 {
		t.Fatalf("zero-key parent must be kept with empty child list: %#v", authors[1])
	}
}

func TestScanGroupsNullVsZeroKey(t *testing.T) {
	columns := []string{"a.code", "a.name", "b.seq", "b.title"}
	rowsData := [][]driver.Value{
		{int64(0), "zero-parent", int64(0), "zero-edition"},
		{int64(0), "zero-parent", nil, nil},
	}
	cfg := GroupConfig{
		ParentPrefix: "a",
		ChildPrefix:  "b",
		ParentKeys:   []string{"a.code"},
		ChildKeys:    []string{"b.seq"},
	}
	db, _ := queryFixture(t, columns, rowsData, nil)
	rows := openRows(t, db)
	var authors []struct {
		Code  int64
		Name  string
		Books []struct {
			Seq   sql.NullInt64
			Title string
		}
	}
	if err := rows.ScanGroups(&authors, cfg); err != nil {
		t.Fatalf("ScanGroups: %v", err)
	}
	if len(authors[0].Books) != 1 {
		t.Fatalf("NULL child key must not collapse onto zero key: %#v", authors[0].Books)
	}
	if !authors[0].Books[0].Seq.Valid || authors[0].Books[0].Seq.Int64 != 0 {
		t.Fatalf("zero key must remain valid and non-NULL: %#v", authors[0].Books[0])
	}
}

// TestScanGroupsNullKeyKeptDistinct exercises a child whose key column is
// NULL while another child column has a value (so the row is not an empty
// LEFT JOIN result). It must remain separate from the zero-valued key.
func TestScanGroupsNullKeyKeptDistinct(t *testing.T) {
	columns := []string{"a.id", "a.name", "b.seq", "b.title"}
	rowsData := [][]driver.Value{
		{int64(1), "jason", int64(0), "zero-key book"},
		{int64(1), "jason", nil, "null-key book"},
	}
	cfg := GroupConfig{
		ParentPrefix: "a",
		ChildPrefix:  "b",
		ParentKeys:   []string{"a.id"},
		ChildKeys:    []string{"b.seq"},
	}
	db, _ := queryFixture(t, columns, rowsData, nil)
	rows := openRows(t, db)
	defer rows.Close()

	var authors []struct {
		ID    int64
		Name  string
		Books []struct {
			Seq   sql.NullInt64
			Title string
		}
	}
	if err := rows.ScanGroups(&authors, cfg); err != nil {
		t.Fatalf("ScanGroups: %v", err)
	}
	if len(authors) != 1 || len(authors[0].Books) != 2 {
		t.Fatalf("NULL and zero keys must be distinct children: %#v", authors)
	}
	if authors[0].Books[0].Seq.Int64 != 0 || !authors[0].Books[0].Seq.Valid {
		t.Fatalf("first child key must be valid zero: %#v", authors[0].Books[0])
	}
	if authors[0].Books[1].Seq.Valid {
		t.Fatalf("second child key must be NULL/invalid: %#v", authors[0].Books[1])
	}
}

func TestScanGroupsFieldMappingError(t *testing.T) {
	columns := append(append([]string{}, authorBookColumns...), "book.unknown")
	rowsData := [][]driver.Value{
		{int64(1), "jason", "east", int64(10), "first light", int64(7)},
	}
	db, _ := queryFixture(t, columns, rowsData, nil)
	rows := openRows(t, db)
	defer rows.Close()

	var authors []gAuthor
	err := rows.ScanGroups(&authors, authorBookCfg)
	if err == nil || !strings.Contains(err.Error(), "missing destination name") {
		t.Fatalf("want missing destination error, got %v", err)
	}
}

func TestScanGroupsUnknownPrefixError(t *testing.T) {
	columns := []string{"author.id", "author.name", "x.region", "book.id", "book.title"}
	rowsData := [][]driver.Value{
		{int64(1), "jason", "east", int64(10), "first light"},
	}
	db, _ := queryFixture(t, columns, rowsData, nil)
	rows := openRows(t, db)
	defer rows.Close()

	var authors []gAuthor
	err := rows.ScanGroups(&authors, authorBookCfg)
	if err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("want prefix error, got %v", err)
	}
}

func TestScanGroupsChildConflict(t *testing.T) {
	rowsData := [][]driver.Value{
		{int64(1), "jason", "east", int64(10), "first light"},
		{int64(1), "jason", "east", int64(10), "CHANGED"},
	}
	db, fixture := queryFixture(t, authorBookColumns, rowsData, nil)
	rows := openRows(t, db)

	original := []gAuthor{{ID: 99, Name: "untouched"}}
	authors := original
	var rowErr *GroupRowError
	err := rows.ScanGroups(&authors, authorBookCfg)
	if !errors.As(err, &rowErr) {
		t.Fatalf("want *GroupRowError, got %T: %v", err, err)
	}
	if rowErr.Row != 2 {
		t.Fatalf("conflict reported at row %d, want 2", rowErr.Row)
	}
	if !strings.Contains(err.Error(), "child key") {
		t.Fatalf("want child conflict error, got %v", err)
	}
	if !reflect.DeepEqual(authors, original) {
		t.Fatalf("dest must be left untouched on failure:\n got %#v\nwant %#v", authors, original)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fixture.rowsCloseCount(); got != 1 {
		t.Fatalf("rows closed %d times, want 1", got)
	}
}

func TestScanGroupsParentConflict(t *testing.T) {
	rowsData := [][]driver.Value{
		{int64(1), "jason", "east", int64(10), "first light"},
		{int64(1), "JASON", "east", nil, nil},
	}
	db, _ := queryFixture(t, authorBookColumns, rowsData, nil)
	rows := openRows(t, db)
	defer rows.Close()

	var authors []gAuthor
	var rowErr *GroupRowError
	err := rows.ScanGroups(&authors, authorBookCfg)
	if !errors.As(err, &rowErr) || rowErr.Row != 2 {
		t.Fatalf("want row-2 parent conflict, got %v", err)
	}
}

func TestScanGroupsScanFailureReportsRow(t *testing.T) {
	// map values are not convertible to int64, so Scan fails.
	rowsData := [][]driver.Value{
		{int64(1), "jason", "east", int64(10), "first light"},
		{int64(2), "brian", "west", map[string]string{"nope": "x"}, "bad"},
	}
	db, _ := queryFixture(t, authorBookColumns, rowsData, nil)
	rows := openRows(t, db)
	defer rows.Close()

	var authors []gAuthor
	var rowErr *GroupRowError
	err := rows.ScanGroups(&authors, authorBookCfg)
	if !errors.As(err, &rowErr) {
		t.Fatalf("want *GroupRowError, got %v", err)
	}
	if rowErr.Row != 2 {
		t.Fatalf("scan failure reported at row %d, want 2", rowErr.Row)
	}
	if len(authors) != 0 {
		t.Fatalf("no partial result may be written, got %#v", authors)
	}
}

func TestScanGroupsRowsErrMidStream(t *testing.T) {
	fixture := &memResult{
		columns: authorBookColumns,
		rows: [][]driver.Value{
			{int64(1), "jason", "east", int64(10), "first light"},
			{int64(2), "brian", "west", int64(20), "meet me"},
		},
		failAfterRow: 1,
		failErr:      errors.New("connection reset"),
	}
	db, fixture := queryFixture(t, authorBookColumns, nil, fixture)
	rows := openRows(t, db)
	defer rows.Close()

	var authors []gAuthor
	var rowErr *GroupRowError
	err := rows.ScanGroups(&authors, authorBookCfg)
	if !errors.As(err, &rowErr) {
		t.Fatalf("want *GroupRowError from Rows.Err, got %v", err)
	}
	if rowErr.Row != 1 {
		t.Fatalf("Rows.Err failure reported at row %d, want 1", rowErr.Row)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("underlying error not wrapped: %v", err)
	}
	if len(authors) != 0 {
		t.Fatalf("no partial result after Rows.Err, got %#v", authors)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fixture.rowsCloseCount(); got != 1 {
		t.Fatalf("rows closed %d times after Rows.Err, want 1", got)
	}
}

func TestScanGroupsPointerSlices(t *testing.T) {
	db, _ := queryFixture(t, authorBookColumns, orderedRows(), nil)
	rows := openRows(t, db)
	defer rows.Close()

	var authors []*gAuthor
	if err := rows.ScanGroups(&authors, authorBookCfg); err != nil {
		t.Fatalf("ScanGroups: %v", err)
	}
	if len(authors) != 2 || authors[0].ID != 2 || authors[1].ID != 1 {
		t.Fatalf("unexpected pointer parents: %#v", authors)
	}
	if len(authors[1].Books) != 1 || authors[1].Books[0].ID != 10 {
		t.Fatalf("unexpected children: %#v", authors[1].Books)
	}
}

func TestScanGroupsPackageLevelWithPlainSqlRows(t *testing.T) {
	db, _ := queryFixture(t, authorBookColumns, orderedRows(), nil)
	sqlRows, err := db.DB.Query("SELECT author_book_join")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var authors []gAuthor
	if err := ScanGroups(sqlRows, &authors, authorBookCfg); err != nil {
		t.Fatalf("ScanGroups with *sql.Rows: %v", err)
	}
	if err := sqlRows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(authors) != 2 {
		t.Fatalf("want 2 authors, got %#v", authors)
	}
}

func TestScanGroupsBadConfig(t *testing.T) {
	db, _ := queryFixture(t, authorBookColumns, orderedRows(), nil)
	rows := openRows(t, db)
	defer rows.Close()

	var authors []gAuthor
	cases := map[string]GroupConfig{
		"empty parent prefix": {
			ChildPrefix: "book", ParentKeys: []string{"author.id"}, ChildKeys: []string{"book.id"},
		},
		"same prefixes": {
			ParentPrefix: "x", ChildPrefix: "x",
			ParentKeys: []string{"x.id"}, ChildKeys: []string{"x.id2"},
		},
		"missing parent key": {
			ParentPrefix: "author", ChildPrefix: "book",
			ParentKeys: []string{"author.missing"}, ChildKeys: []string{"book.id"},
		},
		"key uses wrong prefix": {
			ParentPrefix: "author", ChildPrefix: "book",
			ParentKeys: []string{"book.id"}, ChildKeys: []string{"book.title"},
		},
	}
	for name, cfg := range cases {
		if err := rows.ScanGroups(&authors, cfg); err == nil {
			t.Fatalf("%s: want error, got nil", name)
		}
	}
}

func TestScanGroupsMissingChildSlice(t *testing.T) {
	type noChild struct {
		ID int64
	}
	db, _ := queryFixture(t, authorBookColumns, orderedRows(), nil)
	rows := openRows(t, db)
	defer rows.Close()

	var parents []noChild
	if err := rows.ScanGroups(&parents, authorBookCfg); err == nil ||
		!strings.Contains(err.Error(), "no slice-of-struct field") {
		t.Fatalf("want missing child slice error, got %v", err)
	}
}

func ExampleScanGroups() {
	fixture := &memResult{
		columns: authorBookColumns,
		rows: [][]driver.Value{
			{int64(2), "brian", "west", int64(20), "meet me"},
			{int64(2), "brian", "west", int64(21), "southern"},
			{int64(1), "jason", "east", nil, nil}, // author without books
		},
	}
	db := newMemDB("example-scan-groups", fixture)
	rows, err := db.Queryx("SELECT author_book_join")
	if err != nil {
		fmt.Println("query error:", err)
		return
	}
	defer rows.Close()

	var authors []gAuthor
	if err := ScanGroups(rows, &authors, authorBookCfg); err != nil {
		fmt.Println("scan error:", err)
		return
	}
	for _, a := range authors {
		fmt.Printf("%s:%d", a.Name, len(a.Books))
		for _, b := range a.Books {
			fmt.Printf(" %s", b.Title)
		}
		fmt.Println()
	}
	// Output:
	// brian:2 meet me southern
	// jason:0
}
