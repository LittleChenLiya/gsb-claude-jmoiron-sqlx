package sqlx

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// fakeRows is a test fixture implementing the rowsi interface.  It mimics a
// driver that reuses a single byte buffer across rows: every []byte value it
// vends shares the same backing array, and each Scan overwrites it.
type fakeRows struct {
	cols []string
	data [][]interface{}

	// sharedBuf, when non-nil, is reused as the backing array for every
	// []byte value handed out by Scan, emulating driver buffer reuse.
	sharedBuf []byte

	pos    int // index of the next row to vend
	closed int // number of times Close has been called

	scanErr   error // returned by Scan once scanErrAt row is reached
	scanErrAt int   // row index at which Scan fails; -1 disables
	closeErr  error // returned by Close
	iterErr   error // reported by Err after exhaustion
}

func newFakeRows(cols []string, data [][]interface{}) *fakeRows {
	return &fakeRows{cols: cols, data: data, scanErrAt: -1}
}

func (f *fakeRows) Columns() ([]string, error) { return f.cols, nil }

func (f *fakeRows) Next() bool {
	if f.pos >= len(f.data) {
		return false
	}
	f.pos++
	return true
}

func (f *fakeRows) Scan(dest ...interface{}) error {
	row := f.data[f.pos-1]
	if f.scanErrAt >= 0 && f.pos-1 == f.scanErrAt {
		return f.scanErr
	}
	for i, v := range row {
		if b, ok := v.([]byte); ok && f.sharedBuf != nil {
			// Reuse the shared buffer exactly like a driver would:
			// overwrite its contents and hand out the same array.
			copy(f.sharedBuf, b)
			*(dest[i].(*interface{})) = f.sharedBuf[:len(b)]
			continue
		}
		*(dest[i].(*interface{})) = v
	}
	return nil
}

func (f *fakeRows) Close() error {
	f.closed++
	return f.closeErr
}

func (f *fakeRows) Err() error { return f.iterErr }

// TestMapRowsByteCopy verifies that []byte values are copied into each
// returned map: the fixture reuses one buffer for both rows, so after the
// second row is read the first row's map must still hold its original bytes.
func TestMapRowsByteCopy(t *testing.T) {
	fr := newFakeRows(
		[]string{"id", "payload"},
		[][]interface{}{
			{1, []byte("AAAAAAAA")},
			{2, []byte("BBBBBBBB")},
		},
	)
	fr.sharedBuf = make([]byte, 8)

	it := NewMapRows(fr, 0)

	first, err := it.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	got := string(first["payload"].([]byte))
	if got != "AAAAAAAA" {
		t.Fatalf("first row payload = %q, want %q", got, "AAAAAAAA")
	}

	second, err := it.Next()
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if got := string(second["payload"].([]byte)); got != "BBBBBBBB" {
		t.Fatalf("second row payload = %q, want %q", got, "BBBBBBBB")
	}

	// The driver buffer now holds the second row's bytes; the first map
	// must be unaffected.
	if got := string(first["payload"].([]byte)); got != "AAAAAAAA" {
		t.Fatalf("first row payload after reuse = %q, want %q", got, "AAAAAAAA")
	}

	// Each map must be a distinct object.
	if fmt.Sprintf("%p", first) == fmt.Sprintf("%p", second) {
		t.Fatal("Next returned the same map twice")
	}
}

// TestMapRowsLimitClosesOnce verifies that reaching the row limit releases
// the underlying rows exactly once, and that a later explicit Close does not
// close them again.
func TestMapRowsLimitClosesOnce(t *testing.T) {
	fr := newFakeRows(
		[]string{"id"},
		[][]interface{}{{1}, {2}, {3}},
	)
	it := NewMapRows(fr, 1)

	row, err := it.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if row["id"] != 1 {
		t.Fatalf("row id = %v, want 1", row["id"])
	}
	if fr.closed != 1 {
		t.Fatalf("closes after limit reached = %d, want 1", fr.closed)
	}

	// The iterator is exhausted by the limit.
	if row, err := it.Next(); row != nil || err != nil {
		t.Fatalf("Next past limit = (%v, %v), want (nil, nil)", row, err)
	}
	if fr.closed != 1 {
		t.Fatalf("closes after extra Next = %d, want 1", fr.closed)
	}

	// An explicit Close afterwards must not close the rows again.
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fr.closed != 1 {
		t.Fatalf("closes after explicit Close = %d, want 1", fr.closed)
	}
}

// TestMapRowsScanAndCloseError verifies that when scanning fails and the
// subsequent close also fails, the scan error is kept primary and the close
// error stays attached: both remain identifiable.
func TestMapRowsScanAndCloseError(t *testing.T) {
	scanSentinel := errors.New("scan boom")
	closeSentinel := errors.New("close boom")

	fr := newFakeRows([]string{"id"}, [][]interface{}{{1}})
	fr.scanErr = scanSentinel
	fr.scanErrAt = 0
	fr.closeErr = closeSentinel

	it := NewMapRows(fr, 0)
	row, err := it.Next()
	if row != nil {
		t.Fatalf("row = %v, want nil", row)
	}
	if err == nil {
		t.Fatal("Next error = nil, want combined scan+close error")
	}
	if !errors.Is(err, scanSentinel) {
		t.Fatalf("error %v does not identify scan error %v", err, scanSentinel)
	}
	if !errors.Is(err, closeSentinel) {
		t.Fatalf("error %v does not identify close error %v", err, closeSentinel)
	}

	// The scan error must be primary, the close error attached.
	sce, ok := err.(*ScanCloseError)
	if !ok {
		t.Fatalf("error type = %T, want *ScanCloseError", err)
	}
	if sce.Scan != scanSentinel || sce.Close != closeSentinel {
		t.Fatalf("ScanCloseError = %+v, want scan=%v close=%v", sce, scanSentinel, closeSentinel)
	}

	// Err retains the same combined error; rows were released once.
	if !errors.Is(it.Err(), scanSentinel) || !errors.Is(it.Err(), closeSentinel) {
		t.Fatalf("Err() = %v, want both errors identifiable", it.Err())
	}
	if fr.closed != 1 {
		t.Fatalf("closes = %d, want 1", fr.closed)
	}
}

// TestMapRowsReadOnlyMode verifies that a caller-held (read-only) iterator
// never closes the underlying rows, even on exhaustion or explicit Close.
func TestMapRowsReadOnlyMode(t *testing.T) {
	fr := newFakeRows(
		[]string{"id"},
		[][]interface{}{{1}, {2}},
	)
	it := NewMapRowsReadOnly(fr, 0)

	var ids []interface{}
	for {
		row, err := it.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if row == nil {
			break
		}
		ids = append(ids, row["id"])
	}
	if !reflect.DeepEqual(ids, []interface{}{1, 2}) {
		t.Fatalf("ids = %v, want [1 2]", ids)
	}
	if fr.closed != 0 {
		t.Fatalf("read-only iterator closed rows %d times, want 0", fr.closed)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fr.closed != 0 {
		t.Fatalf("read-only Close closed rows %d times, want 0", fr.closed)
	}

	// The caller still owns the rows and releases them itself.
	if err := fr.Close(); err != nil {
		t.Fatalf("caller Close: %v", err)
	}
	if fr.closed != 1 {
		t.Fatalf("closes = %d, want 1", fr.closed)
	}
}

// TestMapRowsEarlyExit verifies that an explicit Close before the result set
// is exhausted releases the rows exactly once and terminates the iteration.
func TestMapRowsEarlyExit(t *testing.T) {
	fr := newFakeRows(
		[]string{"id"},
		[][]interface{}{{1}, {2}, {3}},
	)
	it := NewMapRows(fr, 0)

	row, err := it.Next()
	if err != nil || row == nil {
		t.Fatalf("Next = (%v, %v), want first row", row, err)
	}
	if fr.closed != 0 {
		t.Fatalf("closes before Close = %d, want 0", fr.closed)
	}

	if err := it.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fr.closed != 1 {
		t.Fatalf("closes after early Close = %d, want 1", fr.closed)
	}

	// Iteration is over even though rows remain.
	if row, err := it.Next(); row != nil || err != nil {
		t.Fatalf("Next after Close = (%v, %v), want (nil, nil)", row, err)
	}
	if fr.closed != 1 {
		t.Fatalf("closes after Next past Close = %d, want 1", fr.closed)
	}
}

// TestMapRowsDoubleClose verifies that Close is idempotent: the underlying
// rows are released exactly once and both calls report the same outcome.
func TestMapRowsDoubleClose(t *testing.T) {
	fr := newFakeRows([]string{"id"}, [][]interface{}{{1}})
	it := NewMapRows(fr, 0)

	if err := it.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if fr.closed != 1 {
		t.Fatalf("closes after double Close = %d, want 1", fr.closed)
	}

	// Double close with a close error: both calls surface the same error.
	closeSentinel := errors.New("close boom")
	fr2 := newFakeRows([]string{"id"}, [][]interface{}{{1}})
	fr2.closeErr = closeSentinel
	it2 := NewMapRows(fr2, 0)
	if err := it2.Close(); !errors.Is(err, closeSentinel) {
		t.Fatalf("first Close = %v, want %v", err, closeSentinel)
	}
	if err := it2.Close(); !errors.Is(err, closeSentinel) {
		t.Fatalf("second Close = %v, want %v", err, closeSentinel)
	}
	if fr2.closed != 1 {
		t.Fatalf("closes after failing double Close = %d, want 1", fr2.closed)
	}
}

// TestMapRowsExhaustion verifies that iterating to the end of the result set
// closes the rows exactly once and reports iteration errors from rows.Err.
func TestMapRowsExhaustion(t *testing.T) {
	fr := newFakeRows(
		[]string{"id"},
		[][]interface{}{{1}, {2}},
	)
	it := NewMapRows(fr, 0)
	n := 0
	for {
		row, err := it.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if row == nil {
			break
		}
		n++
	}
	if n != 2 {
		t.Fatalf("rows yielded = %d, want 2", n)
	}
	if fr.closed != 1 {
		t.Fatalf("closes after exhaustion = %d, want 1", fr.closed)
	}

	// A deferred iteration error surfaces as the terminal error.
	iterSentinel := errors.New("iteration boom")
	fr2 := newFakeRows([]string{"id"}, [][]interface{}{{1}})
	fr2.iterErr = iterSentinel
	it2 := NewMapRows(fr2, 0)
	if _, err := it2.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	row, err := it2.Next()
	if row != nil || !errors.Is(err, iterSentinel) {
		t.Fatalf("Next at exhaustion = (%v, %v), want (nil, %v)", row, err, iterSentinel)
	}
}

// ExampleNewMapRows is a short iteration demo: rows are yielded one map at a
// time and the underlying rows are released automatically at the limit.
func ExampleNewMapRows() {
	fr := newFakeRows(
		[]string{"name", "score"},
		[][]interface{}{
			{"ann", 90},
			{"bob", 80},
			{"cid", 70},
		},
	)
	it := NewMapRows(fr, 2) // read at most two rows
	for {
		row, err := it.Next()
		if err != nil {
			fmt.Println("error:", err)
			break
		}
		if row == nil {
			break
		}
		fmt.Println(row["name"], row["score"])
	}
	fmt.Println("rows closed:", fr.closed)
	// Output:
	// ann 90
	// bob 80
	// rows closed: 1
}
