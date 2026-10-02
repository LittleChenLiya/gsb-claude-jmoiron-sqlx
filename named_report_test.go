package sqlx

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/jmoiron/sqlx/reflectx"
)

func TestCountNamedSideEffectFree(t *testing.T) {
	// counting only; literals, comments and casts are not placeholders
	q := `SELECT :a, 'x:y :z' /* :c */, "id:e", id::text, @v := 1 -- :d
FROM t WHERE k = :a # not :e either`
	names, err := CountNamed(q)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "a"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("CountNamed = %v, want %v", names, want)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 occurrences (repeated :a), got %d", len(names))
	}
}

func TestBindReportMapMissingAndExplicitNil(t *testing.T) {
	q := `select :a,:a,:b`
	r, err := BindNamedReport(QUESTION, q, map[string]interface{}{"a": nil})
	if err != nil {
		t.Fatal(err)
	}

	if r.Query != `select ?,?,?` {
		t.Fatalf("unexpected rewritten query: %q", r.Query)
	}
	if len(r.Placeholders) != 3 {
		t.Fatalf("expected 3 placeholders, got %d", len(r.Placeholders))
	}

	// first two occurrences of a: present, even though value is explicit nil
	if !r.Placeholders[0].Found || !r.Placeholders[1].Found {
		t.Fatalf(":a should be found (explicit nil counts as present): %+v", r.Placeholders)
	}
	if r.Placeholders[0].Name != "a" || r.Placeholders[1].Name != "a" {
		t.Fatalf("expected a twice, got %q %q", r.Placeholders[0].Name, r.Placeholders[1].Name)
	}
	if r.Placeholders[0].Value != nil || r.Placeholders[1].Value != nil {
		t.Fatalf("nil value expected, got %#v", r.Placeholders[0].Value)
	}
	if r.Placeholders[2].Found {
		t.Fatalf(":b should be missing")
	}
	if !r.HasMissing() {
		t.Fatalf("HasMissing should be true")
	}
	if len(r.Missing) != 1 || r.Missing[0] != "b" {
		t.Fatalf("Missing = %v, want [b]", r.Missing)
	}
	if !reflect.DeepEqual(r.Args, []interface{}{nil, nil, nil}) {
		t.Fatalf("Args = %#v, want three nils", r.Args)
	}
}

func TestBindReportNilZeroEmptySliceArePresent(t *testing.T) {
	type args struct {
		A interface{}
		B int
		C []string
	}
	r, err := BindNamedReport(QUESTION, `SELECT :a, :b, :c, :c`, args{
		A: nil,
		B: 0,
		C: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.HasMissing() {
		t.Fatalf("nil/zero/empty-slice must not be missing, got %v", r.Missing)
	}
	for i, p := range r.Placeholders {
		if !p.Found {
			t.Fatalf("placeholder %d (%s) reported missing", i, p.Name)
		}
	}
	if len(r.Args) != 4 {
		t.Fatalf("expected 4 args for repeated :c, got %d", len(r.Args))
	}
}

func TestBindReportLiteralsCommentsCastsUntouched(t *testing.T) {
	q := `SELECT ':x' AS lit, ":y" AS ident, ` + "`z:w`" + ` AS bt,
-- line :c
/* block :d */ id::text, @u := 2 # hash :e
FROM t WHERE a = :ok AND id #in(7)
= :ok`
	r, err := BindNamedReport(QUESTION, q, map[string]interface{}{"ok": 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.HasMissing() {
		t.Fatalf("unexpected missing: %v", r.Missing)
	}
	want := []string{"ok", "ok"}
	got := []string{r.Placeholders[0].Name, r.Placeholders[1].Name}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("placeholders = %v, want %v", got, want)
	}
	// the literal :x etc must survive verbatim, only :ok becomes ?
	wantQuery := `SELECT ':x' AS lit, ":y" AS ident, ` + "`z:w`" + ` AS bt,
-- line :c
/* block :d */ id::text, @u := 2 # hash :e
FROM t WHERE a = ? AND id #in(7)
= ?`
	if r.Query != wantQuery {
		t.Fatalf("rewritten query mismatch:\n got: %q\nwant: %q", r.Query, wantQuery)
	}
}

func TestBindReportCastAfterPlaceholder(t *testing.T) {
	r, err := BindNamedReport(DOLLAR, `SELECT :a::int, :b::text`, map[string]interface{}{
		"a": 1,
		"b": "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := `SELECT $1::int, $2::text`; r.Query != want {
		t.Fatalf("query = %q, want %q", r.Query, want)
	}
}

func TestBindReportRebindAllTypes(t *testing.T) {
	q := `SELECT :a, :b`
	arg := map[string]interface{}{"a": 1, "b": 2}
	cases := map[int]string{
		QUESTION: `SELECT ?, ?`,
		DOLLAR:   `SELECT $1, $2`,
		AT:       `SELECT @p1, @p2`,
		NAMED:    `SELECT :a, :b`,
		UNKNOWN:  `SELECT ?, ?`,
	}
	for bindType, want := range cases {
		r, err := BindNamedReport(bindType, q, arg)
		if err != nil {
			t.Fatal(err)
		}
		if r.Query != want {
			t.Errorf("bindType %d: query = %q, want %q", bindType, r.Query, want)
		}
		if !reflect.DeepEqual(r.Args, []interface{}{1, 2}) {
			t.Errorf("bindType %d: args = %#v", bindType, r.Args)
		}
	}
}

type reportInner struct {
	A string `db:"a"`
}

type reportOuter struct {
	reportInner
	A string `db:"a"` // shadows embedded a per existing reflectx rules
	B string `db:"b"`
}

func TestBindReportStructShadowingFollowsMapper(t *testing.T) {
	arg := reportOuter{
		reportInner: reportInner{A: "inner"},
		A:           "outer",
		B:           "bee",
	}
	m := reflectx.NewMapperFunc("db", nil)
	r, err := bindNamedReportMapper(QUESTION, `SELECT :a, :b, :missing`, arg, m)
	if err != nil {
		t.Fatal(err)
	}

	// reflectx BFS rules make the shallower field (Outer.A) win
	if got := r.Placeholders[0].Value; got != "outer" {
		t.Fatalf("expected outer shadow to win, got %v", got)
	}
	if r.Placeholders[0].Source != "reportOuter.A" {
		t.Fatalf("source = %q, want reportOuter.A", r.Placeholders[0].Source)
	}
	if r.Placeholders[1].Source != "reportOuter.B" {
		t.Fatalf("source = %q, want reportOuter.B", r.Placeholders[1].Source)
	}
	if !r.HasMissing() || r.Missing[0] != "missing" {
		t.Fatalf("missing = %v, want [missing]", r.Missing)
	}

	// and the exact same traversal rules must agree with bindArgs
	names := []string{"a", "b", "missing"}
	if err == nil { // sanity: reflectx lookup for a is index [1]
		travs := m.TraversalsByName(reflect.TypeOf(arg), names[:2])
		if !reflect.DeepEqual(travs[0], []int{1}) {
			t.Fatalf("expected mapper traversal [1] for a, got %v", travs[0])
		}
	}
}

type reportEmbeddedPtr struct {
	*reportInner
	C string `db:"c"`
}

func TestBindReportNilEmbeddedPtrNoAllocation(t *testing.T) {
	arg := reportEmbeddedPtr{C: "see"}
	r, err := BindNamedReport(QUESTION, `SELECT :a, :c`, arg)
	if err != nil {
		t.Fatal(err)
	}
	if r.HasMissing() {
		t.Fatalf("nil embedded pointer field is still present, missing=%v", r.Missing)
	}
	if r.Placeholders[0].Value != nil {
		t.Fatalf("embedded nil pointer value should be nil, got %#v", r.Placeholders[0].Value)
	}
	if arg.reportInner != nil {
		t.Fatalf("report must not allocate the nil embedded pointer")
	}
	if r.Placeholders[1].Value != "see" {
		t.Fatalf("c = %v, want see", r.Placeholders[1].Value)
	}
}

// valuerCounter fails if Value is ever called, proving binding does not run
// user conversion code ahead of time.
type valuerCounter struct{ calls *int }

func (v valuerCounter) Value() (driver.Value, error) {
	*v.calls++
	return nil, errors.New("driver.Valuer.Value must not be called during reporting")
}

func TestBindReportDoesNotCallValuer(t *testing.T) {
	calls := 0
	r, err := BindNamedReport(QUESTION, `SELECT :v, :v`, map[string]interface{}{
		"v": valuerCounter{calls: &calls},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("driver.Valuer.Value called %d times", calls)
	}
	if len(r.Args) != 2 {
		t.Fatalf("repeated placeholder must yield repeated args, got %d", len(r.Args))
	}
}

type reportTagged struct {
	Foo string `db:"foo"`
	Bar int    `db:"bar"`
}

func TestBindReportStructMapAndTags(t *testing.T) {
	r, err := BindNamedReport(DOLLAR, `SELECT :foo, :bar, :foo`, reportTagged{Foo: "x", Bar: 7})
	if err != nil {
		t.Fatal(err)
	}
	if r.Query != `SELECT $1, $2, $3` {
		t.Fatalf("query = %q", r.Query)
	}
	if !reflect.DeepEqual(r.Args, []interface{}{"x", 7, "x"}) {
		t.Fatalf("args = %#v", r.Args)
	}
	if r.Placeholders[0].Source != "reportTagged.Foo" {
		t.Fatalf("source = %q", r.Placeholders[0].Source)
	}

	// pointer to struct is accepted as well
	pr, err := BindNamedReport(QUESTION, `SELECT :foo`, &reportTagged{Foo: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Args[0] != "p" {
		t.Fatalf("pointer struct arg = %#v", pr.Args[0])
	}
}

func TestBindReportUnsupportedType(t *testing.T) {
	if _, err := BindNamedReport(QUESTION, `SELECT :a`, 42); err == nil {
		t.Fatal("expected error for non map/struct argument")
	}
	if _, err := BindNamedReport(QUESTION, `SELECT :a`, nil); err == nil {
		t.Fatal("expected error for nil argument")
	}
}

// ExampleBindNamedReport demonstrates the pure binding inspection entry:
// it performs no database I/O and no driver conversion.
func ExampleBindNamedReport() {
	q := "select :a,:a,:b where ':x' = ':x'"
	r, err := BindNamedReport(QUESTION, q, map[string]interface{}{"a": nil})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(r.Query)
	for _, p := range r.Placeholders {
		fmt.Printf("%d %s found=%t source=%q\n", p.Order, p.Name, p.Found, p.Source)
	}
	fmt.Println("missing:", r.Missing)
	// Output:
	// select ?,?,? where ':x' = ':x'
	// 0 a found=true source="a"
	// 1 a found=true source="a"
	// 2 b found=false source=""
	// missing: [b]
}
