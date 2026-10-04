package sqlx

import "fmt"

// MapRowsMode controls when a MapRows iterator releases the underlying rows.
type MapRowsMode int

const (
	// MapRowsAutoClose is the automatic mode: the iterator releases the
	// underlying rows as soon as the row limit is reached. Close may still be
	// called afterwards and is idempotent.
	MapRowsAutoClose MapRowsMode = iota
	// MapRowsHold is the caller-held mode: the iterator never releases the
	// rows on its own; they are released only when Close is called explicitly.
	// This keeps the caller in control of the rows lifetime.
	MapRowsHold
)

// MapRows is a forward-only iterator over a result set that yields each row as
// a freshly allocated map[string]interface{}.
//
// Unlike collecting every row into a slice of maps up front, MapRows scans one
// row at a time. After a successful Next call the current row is available via
// Map. The returned map and its values are owned by the caller: []byte values
// handed back by the driver are copied, so a driver that reuses a single byte
// buffer across scans cannot mutate maps already produced.
//
// MapRows is safe for a single goroutine. It must be released with Close once;
// in auto-close mode the underlying rows are additionally released
// automatically when the row limit is reached. Close is idempotent in both
// modes.
type MapRows struct {
	rows      rowsi
	columns   []string
	values    []interface{}
	limit     int
	count     int
	autoClose bool
	closed    bool
	scanErr   error
	closeErr  error
}

// MapRows returns a new MapRows iterator over r.
//
// If limit > 0, at most limit rows are returned. In MapRowsAutoClose mode the
// underlying rows are released automatically once the limit is reached; in
// MapRowsHold mode they are released only when Close is called explicitly.
//
// If limit <= 0, all rows are iterated and the rows are released only via
// Close (or by database/sql when the rows are exhausted).
func (r *Rows) MapRows(limit int, mode MapRowsMode) *MapRows {
	it := &MapRows{rows: r, autoClose: mode == MapRowsAutoClose}
	if limit > 0 {
		it.limit = limit
	}
	return it
}

// Next advances the iterator to the next row. It returns false when the limit
// has been reached, the rows are exhausted, or a scan error occurs. In
// auto-close mode the underlying rows are released automatically as soon as
// the limit is reached; every other release path goes through Close.
func (it *MapRows) Next() bool {
	if it.closed {
		return false
	}
	if it.limit > 0 && it.count >= it.limit {
		it.autoRelease()
		return false
	}
	if !it.rows.Next() {
		// database/sql closes the rows on EOF or on a driver error. Capture
		// any scan error so it can be reported together with a close error.
		if err := it.rows.Err(); err != nil {
			it.scanErr = err
		}
		return false
	}
	if it.columns == nil {
		columns, err := it.rows.Columns()
		if err != nil {
			it.scanErr = err
			return false
		}
		it.columns = columns
		it.values = make([]interface{}, len(columns))
		for i := range it.values {
			it.values[i] = new(interface{})
		}
	}
	if err := it.rows.Scan(it.values...); err != nil {
		it.scanErr = err
		return false
	}
	it.count++
	return true
}

// autoRelease releases the underlying rows exactly once, but only in
// auto-close mode. It is a no-op in caller-held mode and after Close.
func (it *MapRows) autoRelease() {
	if !it.autoClose || it.closed {
		return
	}
	it.closed = true
	it.closeErr = it.rows.Close()
}

// Map returns the current row as a new map[string]interface{}. Every call
// allocates a fresh map; []byte values are copied so that driver buffer reuse
// on subsequent scans cannot mutate them. Map must only be called after a
// successful Next call.
func (it *MapRows) Map() map[string]interface{} {
	m := make(map[string]interface{}, len(it.columns))
	for i, column := range it.columns {
		v := *(it.values[i].(*interface{}))
		// Drivers commonly return []byte backed by a buffer that is reused on
		// the next scan. Copy the bytes so the map keeps an independent value.
		if b, ok := v.([]byte); ok {
			cp := make([]byte, len(b))
			copy(cp, b)
			v = cp
		}
		m[column] = v
	}
	return m
}

// Close releases the iterator resources and the underlying rows. It is
// idempotent: calling it more than once never closes the rows twice. In
// auto-close mode the rows may already have been released at the limit, in
// which case Close is a no-op for the underlying rows.
//
// If a scan error occurred it is reported first; if closing the rows also
// failed the close error is appended. Both errors are reachable via errors.Is
// and errors.As on the returned value.
func (it *MapRows) Close() error {
	if !it.closed {
		it.closed = true
		it.closeErr = it.rows.Close()
	}
	return it.err()
}

// err combines the scan error and the close error, keeping the scan error
// first.
func (it *MapRows) err() error {
	if it.scanErr != nil && it.closeErr != nil {
		return &mapRowsError{scanErr: it.scanErr, closeErr: it.closeErr}
	}
	if it.scanErr != nil {
		return it.scanErr
	}
	return it.closeErr
}

// mapRowsError is the combined error returned by Close when both scanning and
// closing the rows fail. The scan error is kept first; both errors are
// reachable via errors.Is and errors.As.
type mapRowsError struct {
	scanErr  error
	closeErr error
}

func (e *mapRowsError) Error() string {
	return fmt.Sprintf("sqlx: maprows: scan error: %v; close error: %v", e.scanErr, e.closeErr)
}

// Unwrap exposes the scan error and the close error so that errors.Is and
// errors.As can identify either one.
func (e *mapRowsError) Unwrap() []error {
	return []error{e.scanErr, e.closeErr}
}
