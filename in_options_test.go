package sqlx

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// countingValuer records how many times Value is called.
type countingValuer struct {
	calls int
	v     driver.Value
}

func (c *countingValuer) Value() (driver.Value, error) {
	c.calls++
	return c.v, nil
}

func TestInWithOptionsEmptySliceNull(t *testing.T) {
	query := "select * from t where id in (?) and tag=?"
	q, args, err := InWithOptions(query, InOptions{EmptySlice: EmptySliceNull}, []int{}, "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "select * from t where id in (NULL) and tag=?"
	if q != want {
		t.Errorf("got  %q\nwant %q", q, want)
	}
	if len(args) != 1 || args[0] != "x" {
		t.Errorf("expected only the tag argument to remain, got %#v", args)
	}
}

func TestInWithOptionsEmptySliceError(t *testing.T) {
	query := "select * from t where id in (?) and tag=?"
	q, args, err := InWithOptions(query, InOptions{EmptySlice: EmptySliceError}, []int{}, "x")
	if err == nil {
		t.Fatal("expected an error for empty slice with EmptySliceError policy")
	}
	if !errors.Is(err, ErrEmptySlice) {
		t.Errorf("expected error to wrap ErrEmptySlice, got %v", err)
	}
	if q != "" || args != nil {
		t.Errorf("expected no partial query/args on failure, got %q %#v", q, args)
	}

	// the zero InOptions value rejects empty slices, matching In
	if _, _, err := InWithOptions(query, InOptions{}, []int{}, "x"); !errors.Is(err, ErrEmptySlice) {
		t.Errorf("zero InOptions should reject empty slices, got %v", err)
	}
}

func TestInWithOptionsEmptySliceNullMixed(t *testing.T) {
	q, args, err := InWithOptions(
		"select * from t where a in (?) and b in (?) and c=?",
		InOptions{EmptySlice: EmptySliceNull},
		[]int{1, 2}, []string{}, "z",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "select * from t where a in (?, ?) and b in (NULL) and c=?"
	if q != want {
		t.Errorf("got  %q\nwant %q", q, want)
	}
	if !reflect.DeepEqual(args, []interface{}{1, 2, "z"}) {
		t.Errorf("unexpected args: %#v", args)
	}
}

func TestInWithOptionsByteSliceNotExpanded(t *testing.T) {
	q, args, err := InWithOptions(
		"select * from t where hash=? and id in (?)",
		InOptions{EmptySlice: EmptySliceNull},
		[]byte{1, 2, 3}, []int{7, 8},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "select * from t where hash=? and id in (?, ?)"
	if q != want {
		t.Errorf("got  %q\nwant %q", q, want)
	}
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %#v", args)
	}
	if b, ok := args[0].([]byte); !ok || !reflect.DeepEqual(b, []byte{1, 2, 3}) {
		t.Errorf("expected []byte to stay a single value, got %#v", args[0])
	}
}

func TestInWithOptionsValuerCalledOnce(t *testing.T) {
	scalar := &countingValuer{v: "tag"}
	slice := &countingValuer{v: []int{1, 2, 3}}

	q, args, err := InWithOptions(
		"select * from t where id in (?) and tag=?",
		InOptions{EmptySlice: EmptySliceNull},
		slice, scalar,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slice.calls != 1 {
		t.Errorf("slice valuer called %d times, want exactly 1", slice.calls)
	}
	if scalar.calls != 1 {
		t.Errorf("scalar valuer called %d times, want exactly 1", scalar.calls)
	}
	want := "select * from t where id in (?, ?, ?) and tag=?"
	if q != want {
		t.Errorf("got  %q\nwant %q", q, want)
	}
	if !reflect.DeepEqual(args, []interface{}{1, 2, 3, "tag"}) {
		t.Errorf("unexpected args: %#v", args)
	}
}

func TestInWithOptionsQuestionMarkInStringLiteral(t *testing.T) {
	query := `select * from t where note='what?' and id in (?) and tag=?`
	q, args, err := InWithOptions(query, InOptions{EmptySlice: EmptySliceNull}, []int{4, 5}, "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `select * from t where note='what?' and id in (?, ?) and tag=?`
	if q != want {
		t.Errorf("got  %q\nwant %q", q, want)
	}
	if !reflect.DeepEqual(args, []interface{}{4, 5, "x"}) {
		t.Errorf("unexpected args: %#v", args)
	}

	// doubled quotes and backslash escapes inside literals must not end them early
	tricky := "select 'it''s ?' , 'a\\'b ?' , `tick?` , id from t where id in (?)"
	q, args, err = InWithOptions(tricky, InOptions{EmptySlice: EmptySliceNull}, []int{9})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want = "select 'it''s ?' , 'a\\'b ?' , `tick?` , id from t where id in (?)"
	if q != want {
		t.Errorf("got  %q\nwant %q", q, want)
	}
	if !reflect.DeepEqual(args, []interface{}{9}) {
		t.Errorf("unexpected args: %#v", args)
	}
}

func TestInWithOptionsArgCountMismatch(t *testing.T) {
	if _, _, err := InWithOptions("select * from t where a=? and b=?", InOptions{}, 1); err == nil {
		t.Error("expected error when bindVars exceed arguments")
	}
	if _, _, err := InWithOptions("select * from t where a=?", InOptions{}, 1, 2); err == nil {
		t.Error("expected error when arguments exceed bindVars")
	}
}

func TestInWithOptionsDoesNotMutateArgs(t *testing.T) {
	cv := &countingValuer{v: "resolved"}
	args := []interface{}{[]int{1, 2}, cv}
	orig := make([]interface{}, len(args))
	copy(orig, args)

	if _, _, err := InWithOptions("select * from t where id in (?) and tag=?", InOptions{}, args...); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(args, orig) {
		t.Errorf("input args slice was modified: %#v", args)
	}
	if args[1] != cv {
		t.Errorf("valuer argument was replaced in caller's slice: %#v", args[1])
	}
}

func TestInWithOptionsNoSlices(t *testing.T) {
	query := "select * from t where a=? and b=?"
	args := []interface{}{1, 2}
	q, out, err := InWithOptions(query, InOptions{}, args...)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q != query {
		t.Errorf("query changed: %q", q)
	}
	if !reflect.DeepEqual(out, args) {
		t.Errorf("args changed: %#v", out)
	}
}

func TestInStillRejectsEmptySlices(t *testing.T) {
	// the original In keeps its historical behavior
	if _, _, err := In("select * from t where id in (?)", []int{}); err == nil {
		t.Error("expected In to keep rejecting empty slices")
	}
}

func ExampleInWithOptions() {
	// NULL policy: an empty id list becomes a single SQL NULL and
	// contributes no bindvar, so only the tag argument remains.
	query := "select * from users where id in (?) and tag=?"
	q, args, err := InWithOptions(query, InOptions{EmptySlice: EmptySliceNull}, []int{}, "admin")
	fmt.Println(q)
	fmt.Println(args, err)

	// Reject policy: the empty slice is a diagnosable error.
	_, _, err = InWithOptions(query, InOptions{EmptySlice: EmptySliceError}, []int{}, "admin")
	fmt.Println(errors.Is(err, ErrEmptySlice), err)

	// Question marks inside string literals are not bindvars.
	q, args, _ = InWithOptions("select * from t where note='really?' and id in (?)",
		InOptions{EmptySlice: EmptySliceNull}, []int{7, 8})
	fmt.Println(q)
	fmt.Println(args)

	// Output:
	// select * from users where id in (NULL) and tag=?
	// [admin] <nil>
	// true sqlx: empty slice passed to 'in' query: argument 0
	// select * from t where note='really?' and id in (?, ?)
	// [7 8]
}
