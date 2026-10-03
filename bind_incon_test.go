package sqlx

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestInConEmptyNull: an empty slice with the NULL strategy turns the
// placeholder into a single SQL NULL and contributes no bind values.
func TestInConEmptyNull(t *testing.T) {
	query := "where id in (?) and tag=?"
	args := []interface{}{[]int{}, "x"}

	q, out, err := InCon(query, args, InOptions{Empty: InEmptyNull})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "where id in (NULL) and tag=?"; q != want {
		t.Errorf("query = %q, want %q", q, want)
	}
	if len(out) != 1 || out[0] != "x" {
		t.Errorf("args = %#v, want [x]", out)
	}
	// the input slice must not have been modified
	if len(args) != 2 || len(args[0].([]int)) != 0 || args[1] != "x" {
		t.Errorf("input args modified: %#v", args)
	}
}

// TestInConEmptyReject: an empty slice with the reject strategy returns a
// diagnosable error and no partial query/args.
func TestInConEmptyReject(t *testing.T) {
	query := "where id in (?) and tag=?"
	args := []interface{}{[]int{}, "x"}

	q, out, err := InCon(query, args, InOptions{Empty: InEmptyReject})
	if err == nil {
		t.Fatal("expected an error for empty slice, got nil")
	}
	if !strings.Contains(err.Error(), "empty slice") {
		t.Errorf("error %q does not mention 'empty slice'", err)
	}
	if q != "" {
		t.Errorf("expected empty query on failure, got %q", q)
	}
	if out != nil {
		t.Errorf("expected nil args on failure, got %#v", out)
	}
	// the input slice must not have been modified
	if len(args) != 2 || len(args[0].([]int)) != 0 || args[1] != "x" {
		t.Errorf("input args modified: %#v", args)
	}
}

// TestInConDefaultRejects: the zero-value InOptions (and the legacy In)
// reject empty slices.
func TestInConDefaultRejects(t *testing.T) {
	if InEmptyReject != 0 {
		t.Errorf("InEmptyReject must be the zero value, got %d", InEmptyReject)
	}
	_, _, err := InCon("where id in (?)", []interface{}{[]int{}}, InOptions{})
	if err == nil {
		t.Fatal("expected error for zero-value options, got nil")
	}
}

// TestInConByteSlice: []byte is a driver.Value type and must stay a single
// value, never be expanded.
func TestInConByteSlice(t *testing.T) {
	query := "where blob in (?) and id in (?)"
	args := []interface{}{[]byte("foo"), []int{1, 2, 3}}

	q, out, err := InCon(query, args, InOptions{Empty: InEmptyReject})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "where blob in (?) and id in (?, ?, ?)"; q != want {
		t.Errorf("query = %q, want %q", q, want)
	}
	if len(out) != 4 {
		t.Fatalf("args = %#v, want 4 values", out)
	}
	if b, ok := out[0].([]byte); !ok || string(b) != "foo" {
		t.Errorf("first arg = %#v, want []byte(\"foo\")", out[0])
	}
	if !reflect.DeepEqual(out[1:], []interface{}{1, 2, 3}) {
		t.Errorf("expanded args = %#v, want [1 2 3]", out[1:])
	}
}

// countingValuer records how many times Value() is called.
type countingValuer struct {
	calls int
	val   driver.Value
}

func (c *countingValuer) Value() (driver.Value, error) {
	c.calls++
	return c.val, nil
}

// TestInConValuerCalledOnce: a driver.Valuer is consulted exactly once when
// classifying its value as a slice; it must not be called per element.
func TestInConValuerCalledOnce(t *testing.T) {
	cv := &countingValuer{val: []int{10, 20, 30}}

	q, out, err := InCon("where id in (?)", []interface{}{cv}, InOptions{Empty: InEmptyReject})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cv.calls != 1 {
		t.Errorf("Value() called %d times, want exactly 1", cv.calls)
	}
	if want := "where id in (?, ?, ?)"; q != want {
		t.Errorf("query = %q, want %q", q, want)
	}
	if !reflect.DeepEqual(out, []interface{}{10, 20, 30}) {
		t.Errorf("args = %#v, want [10 20 30]", out)
	}

	// a Valuer returning a scalar also counts exactly one call
	cv2 := &countingValuer{val: "scalar"}
	_, out2, err := InCon("where name = ?", []interface{}{cv2}, InOptions{Empty: InEmptyReject})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cv2.calls != 1 {
		t.Errorf("Value() called %d times, want exactly 1", cv2.calls)
	}
	if len(out2) != 1 || out2[0] != "scalar" {
		t.Errorf("args = %#v, want [scalar]", out2)
	}
}

// TestInConValuerEmptyNull: a Valuer returning an empty slice with the NULL
// strategy becomes NULL and Value() is still called exactly once.
func TestInConValuerEmptyNull(t *testing.T) {
	cv := &countingValuer{val: []int{}}

	q, out, err := InCon("where id in (?) and tag=?", []interface{}{cv, "t"}, InOptions{Empty: InEmptyNull})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cv.calls != 1 {
		t.Errorf("Value() called %d times, want exactly 1", cv.calls)
	}
	if want := "where id in (NULL) and tag=?"; q != want {
		t.Errorf("query = %q, want %q", q, want)
	}
	if len(out) != 1 || out[0] != "t" {
		t.Errorf("args = %#v, want [t]", out)
	}
}

// TestInConQuestionMarkInQuotes: a '?' inside a quoted string or identifier
// is not treated as a placeholder.
func TestInConQuestionMarkInQuotes(t *testing.T) {
	tests := []struct {
		query string
		args  []interface{}
		want  string
	}{
		{
			"where note = 'what?' and id in (?)",
			[]interface{}{[]int{1, 2, 3}},
			"where note = 'what?' and id in (?, ?, ?)",
		},
		{
			"where note = \"what?\" and id in (?)",
			[]interface{}{[]int{1, 2}},
			"where note = \"what?\" and id in (?, ?)",
		},
		{
			"where `weird?col` = ? and id in (?)",
			[]interface{}{"v", []int{9}},
			"where `weird?col` = ? and id in (?)",
		},
		{
			"where note = 'it''s a ?' and id in (?)",
			[]interface{}{[]int{1}},
			"where note = 'it''s a ?' and id in (?)",
		},
		{
			`where note = 'escaped \'?\'' and id in (?)`,
			[]interface{}{[]int{1}},
			`where note = 'escaped \'?\'' and id in (?)`,
		},
	}
	for i, test := range tests {
		q, _, err := InCon(test.query, test.args, InOptions{Empty: InEmptyReject})
		if err != nil {
			t.Errorf("test %d: unexpected error: %v", i, err)
			continue
		}
		if q != test.want {
			t.Errorf("test %d: query = %q, want %q", i, q, test.want)
		}
	}
}

// TestInConPlaceholderCount: bind variable count is checked against the
// number of arguments before any SQL is produced.
func TestInConPlaceholderCount(t *testing.T) {
	tests := []struct {
		query string
		args  []interface{}
	}{
		{"where id in (?) and tag=?", []interface{}{[]int{1}}},
		{"where id in (?)", []interface{}{[]int{1}, "extra"}},
		{"where id = ? and name = ?", []interface{}{[]int{1, 2}}},
	}
	for i, test := range tests {
		q, out, err := InCon(test.query, test.args, InOptions{Empty: InEmptyNull})
		if err == nil {
			t.Errorf("test %d: expected error, got nil", i)
			continue
		}
		if q != "" || out != nil {
			t.Errorf("test %d: expected empty query and nil args, got %q / %#v", i, q, out)
		}
	}
}

// TestInConNoSlices: a query with no slices is still validated and rewritten
// with placeholders left intact.
func TestInConNoSlices(t *testing.T) {
	query := "where a = ? and b = ?"
	q, out, err := InCon(query, []interface{}{1, "x"}, InOptions{Empty: InEmptyReject})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q != query {
		t.Errorf("query = %q, want %q", q, query)
	}
	if !reflect.DeepEqual(out, []interface{}{1, "x"}) {
		t.Errorf("args = %#v, want [1 x]", out)
	}
}

// TestInConInputNotMutatedOnFailure: a failed call leaves the input slice
// completely untouched.
func TestInConInputNotMutatedOnFailure(t *testing.T) {
	args := []interface{}{[]int{1, 2}, "x"}
	orig := append([]interface{}(nil), args...)

	if _, _, err := InCon("where id in (?)", args, InOptions{Empty: InEmptyReject}); err == nil {
		t.Fatal("expected error")
	}
	if !reflect.DeepEqual(args, orig) {
		t.Errorf("input mutated: %#v, want %#v", args, orig)
	}
}

// ExampleInCon is a print-only demo: it shows how InCon rewrites a query
// without connecting to a database.
func ExampleInCon() {
	query := "where id in (?) and tag=?"

	// Empty slice with the NULL strategy: IN (NULL), only the tag remains.
	q, args, err := InCon(query, []interface{}{[]int{}, "tag"}, InOptions{Empty: InEmptyNull})
	fmt.Printf("null:  %s | %v | %v\n", q, args, err)

	// Empty slice with the reject strategy: a diagnosable error, no output.
	q, args, err = InCon(query, []interface{}{[]int{}, "tag"}, InOptions{Empty: InEmptyReject})
	fmt.Printf("reject: err=%v query=%q args=%v\n", err, q, args)

	// A non-empty slice expands to one placeholder per element.
	q, args, err = InCon("where id in (?) and tag=?", []interface{}{[]int{1, 2, 3}, "tag"}, InOptions{Empty: InEmptyReject})
	fmt.Printf("expand: %s | %v | %v\n", q, args, err)

	// A '?' inside a quoted string is not a placeholder.
	q, args, err = InCon("where note = 'what?' and id in (?)", []interface{}{[]int{7}}, InOptions{Empty: InEmptyReject})
	fmt.Printf("quote: %s | %v | %v\n", q, args, err)

	// Output:
	// null:  where id in (NULL) and tag=? | [tag] | <nil>
	// reject: err=InCon: empty slice passed to 'in' query at argument 0 query="" args=[]
	// expand: where id in (?, ?, ?) and tag=? | [1 2 3 tag] | <nil>
	// quote: where note = 'what?' and id in (?) | [7] | <nil>
}
