package sqlx

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
)

// The acceptance case: select :a,:a,:b with a explicitly nil and b missing.
func TestReportNamedMap(t *testing.T) {
	query := `select :a,:a,:b`
	arg := map[string]interface{}{"a": nil}

	report, err := ReportNamed(query, arg)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Params) != 3 {
		t.Fatalf("expected 3 params, got %d: %#v", len(report.Params), report.Params)
	}
	want := []NamedParam{
		{Name: "a", Index: 0, Found: true, Source: NamedParamSourceMap, Value: nil},
		{Name: "a", Index: 1, Found: true, Source: NamedParamSourceMap, Value: nil},
		{Name: "b", Index: 2, Found: false, Source: ""},
	}
	for i, w := range want {
		got := report.Params[i]
		if got.Name != w.Name || got.Index != w.Index || got.Found != w.Found || got.Source != w.Source {
			t.Errorf("param %d: expected %#v, got %#v", i, w, got)
		}
	}
	if report.Found != 2 {
		t.Errorf("expected 2 found, got %d", report.Found)
	}
	if !report.HasMissing() {
		t.Errorf("expected missing params, got none")
	}
	if len(report.Missing) != 1 || report.Missing[0] != "b" {
		t.Errorf("expected missing [b], got %#v", report.Missing)
	}
	if report.Bound != `select ?,?,?` {
		t.Errorf("expected bound `select ?,?,?`, got %q", report.Bound)
	}
}

// Nil, zero values and empty slices are explicitly present and must not be
// reported as missing.
func TestReportNamedZeroValues(t *testing.T) {
	arg := map[string]interface{}{
		"nil":   nil,
		"zero":  0,
		"empty": "",
		"slice": []int{},
	}
	report, err := ReportNamed(`select :nil, :zero, :empty, :slice, :gone`, arg)
	if err != nil {
		t.Fatal(err)
	}
	if report.Found != 4 {
		t.Errorf("expected 4 found, got %d", report.Found)
	}
	if len(report.Missing) != 1 || report.Missing[0] != "gone" {
		t.Errorf("expected only [gone] missing, got %#v", report.Missing)
	}
}

// A named map type convertible to map[string]interface{} works just like a
// plain map.
func TestReportNamedTypedMap(t *testing.T) {
	type params map[string]interface{}
	arg := params{"a": 1}
	report, err := ReportNamed(`select :a, :b`, arg)
	if err != nil {
		t.Fatal(err)
	}
	if report.Found != 1 || len(report.Missing) != 1 || report.Missing[0] != "b" {
		t.Errorf("unexpected report: found=%d missing=%#v", report.Found, report.Missing)
	}
}

// reportInner is embedded anonymously into reportOuter to exercise the
// reflectx field-shadowing rules.
type reportInner struct {
	InnerOnly string
	A         int
}

type reportOuter struct {
	reportInner
	A string // shadows the embedded reportInner.A
	B bool
}

func TestReportNamedStruct(t *testing.T) {
	arg := reportOuter{B: true}
	query := `select :a, :b, :inneronly, :missing`

	report, err := ReportNamed(query, arg)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Params) != 4 {
		t.Fatalf("expected 4 params, got %d: %#v", len(report.Params), report.Params)
	}

	// :a resolves to the top-level A (string), which shadows the embedded A.
	p := report.Params[0]
	if !p.Found || p.Source != NamedParamSourceStruct || p.Name != "a" {
		t.Errorf("param 0: expected struct field a, got %#v", p)
	}
	if v, ok := p.Value.(string); !ok || v != "" {
		t.Errorf("param 0: expected top-level A value %q, got %#v", "", p.Value)
	}

	// :b resolves to the top-level B.
	p = report.Params[1]
	if !p.Found || p.Source != NamedParamSourceStruct {
		t.Errorf("param 1: expected struct field b, got %#v", p)
	}
	if v, ok := p.Value.(bool); !ok || !v {
		t.Errorf("param 1: expected B value true, got %#v", p.Value)
	}

	// :InnerOnly resolves to the embedded field.
	p = report.Params[2]
	if !p.Found || p.Source != NamedParamSourceStruct {
		t.Errorf("param 2: expected embedded field InnerOnly, got %#v", p)
	}

	// :missing is not a field.
	p = report.Params[3]
	if p.Found || p.Source != "" {
		t.Errorf("param 3: expected missing, got %#v", p)
	}
	if len(report.Missing) != 1 || report.Missing[0] != "missing" {
		t.Errorf("expected missing [missing], got %#v", report.Missing)
	}
	if report.Found != 3 {
		t.Errorf("expected 3 found, got %d", report.Found)
	}
}

func TestReportNamedStructPointer(t *testing.T) {
	arg := &reportOuter{B: true}
	report, err := ReportNamed(`select :b`, arg)
	if err != nil {
		t.Fatal(err)
	}
	if report.Found != 1 {
		t.Errorf("expected 1 found for pointer arg, got %d", report.Found)
	}
}

// countingValuer records whether Value was called.
type countingValuer struct {
	calls int
}

func (c *countingValuer) Value() (driver.Value, error) {
	c.calls++
	return nil, errors.New("Value must not be called during a report")
}

// ReportNamed must never call driver.Valuer.Value: it only binds, it does
// not execute user code.
func TestReportNamedNoSideEffects(t *testing.T) {
	v := &countingValuer{}

	mapReport, err := ReportNamed(`select :a`, map[string]interface{}{"a": v})
	if err != nil {
		t.Fatal(err)
	}
	if v.calls != 0 {
		t.Errorf("map: driver.Valuer.Value was called %d times", v.calls)
	}
	if mapReport.Params[0].Value != v {
		t.Errorf("map: expected the original valuer as value, got %#v", mapReport.Params[0].Value)
	}

	v2 := &countingValuer{}
	structReport, err := ReportNamed(`select :a`, &struct{ A *countingValuer }{A: v2})
	if err != nil {
		t.Fatal(err)
	}
	if v2.calls != 0 {
		t.Errorf("struct: driver.Valuer.Value was called %d times", v2.calls)
	}
	if structReport.Params[0].Value != v2 {
		t.Errorf("struct: expected the original valuer as value, got %#v", structReport.Params[0].Value)
	}
}

// The report's Bound query uses ? bindvars and can be rebound to any
// supported bind type.
func TestReportNamedRebind(t *testing.T) {
	query := `select :a, :b`
	report, err := ReportNamed(query, map[string]interface{}{"a": 1, "b": 2})
	if err != nil {
		t.Fatal(err)
	}
	if report.Bound != `select ?, ?` {
		t.Errorf("expected `select ?, ?`, got %q", report.Bound)
	}
	if got := Rebind(DOLLAR, report.Bound); got != `select $1, $2` {
		t.Errorf("DOLLAR: expected `select $1, $2`, got %q", got)
	}
	if got := Rebind(NAMED, report.Bound); got != `select :arg1, :arg2` {
		t.Errorf("NAMED: expected `select :arg1, :arg2`, got %q", got)
	}
	if got := Rebind(AT, report.Bound); got != `select @p1, @p2` {
		t.Errorf("AT: expected `select @p1, @p2`, got %q", got)
	}

	// Duplicate placeholders still produce one ? per occurrence in Bound.
	dup, err := ReportNamed(`select :a, :a, :b`, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if dup.Bound != `select ?, ?, ?` {
		t.Errorf("duplicate: expected `select ?, ?, ?`, got %q", dup.Bound)
	}
	if len(dup.Params) != 3 || dup.Params[0].Name != "a" || dup.Params[1].Name != "a" || dup.Params[2].Name != "b" {
		t.Errorf("duplicate: expected params [a a b], got %#v", dup.Params)
	}
}

// Colons inside SQL string literals, quoted identifiers, comments and ::
// type casts must not be misidentified as named parameters.
func TestReportNamedLiteralsAndComments(t *testing.T) {
	cases := []struct {
		query string
		bound string
		n     int
	}{
		{`select ':x'`, `select ':x'`, 0},
		{`select :a, ':x'`, `select ?, ':x'`, 1},
		{`select :a -- :nope`, `select ? -- :nope`, 1},
		{`select /* :nope */ :a`, `select /* :nope */ ?`, 1},
		{`select /* outer /* :nope */ still */ :a`, `select /* outer /* :nope */ still */ ?`, 1},
		{`select :a::int`, `select ?::int`, 1},
		{`select :a := 1`, `select ? := 1`, 1},
		{`select "x:y"`, `select "x:y"`, 0},
		{"select `x:y`", "select `x:y`", 0},
		{`select :a, :a, :b`, `select ?, ?, ?`, 3},
		{`select :a, :a, :b -- :c`, `select ?, ?, ? -- :c`, 3},
		{`select 'it''s :x'`, `select 'it''s :x'`, 0},
		{`select 'it\'s :x'`, `select 'it\'s :x'`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			report, err := ReportNamed(tc.query, map[string]interface{}{})
			if err != nil {
				t.Fatal(err)
			}
			if report.Bound != tc.bound {
				t.Errorf("expected bound %q, got %q", tc.bound, report.Bound)
			}
			if len(report.Params) != tc.n {
				t.Errorf("expected %d params, got %d: %#v", tc.n, len(report.Params), report.Params)
			}
		})
	}
}

func TestCountNamed(t *testing.T) {
	cases := map[string]int{
		`select :a, :a, :b`:       3,
		`select ':x'`:             0,
		`select :a, ':x'`:         1,
		`select :a -- :nope`:      1,
		`select /* :nope */ :a`:   1,
		`select :a::int`:          1,
		`select "x:y"`:            0,
		`select :a, :a, :b -- :c`: 3,
		``:                        0,
	}
	for query, want := range cases {
		if got := CountNamed(query); got != want {
			t.Errorf("CountNamed(%q) = %d, want %d", query, got, want)
		}
	}
}

// ExampleReportNamed demonstrates a pure binding report: it never touches
// the database and never executes user code.
func ExampleReportNamed() {
	query := `select :a, :a, :b`
	arg := map[string]interface{}{"a": nil}

	report, err := ReportNamed(query, arg)
	if err != nil {
		panic(err)
	}

	fmt.Println("bound query:", report.Bound)
	for _, p := range report.Params {
		if p.Found {
			fmt.Printf("param %d: %s found (source: %s)\n", p.Index, p.Name, p.Source)
		} else {
			fmt.Printf("param %d: %s MISSING\n", p.Index, p.Name)
		}
	}
	fmt.Println("missing:", report.Missing)

	// Output:
	// bound query: select ?, ?, ?
	// param 0: a found (source: map)
	// param 1: a found (source: map)
	// param 2: b MISSING
	// missing: [b]
}
