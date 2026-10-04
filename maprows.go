// Package sqlx extension: MapRows, a lazy row iterator that yields each row
// as an independent map[string]interface{} without collecting all rows up
// front.
//
// MapRows is designed for reading large result sets row by row.  Unlike
// MapScan (which scans a single row into a caller supplied map and is left
// unmodified), MapRows owns the iteration lifecycle:
//
//   - Every call to Next returns a freshly allocated map.  []byte values are
//     copied before being stored, so a driver that reuses its scan buffer
//     cannot corrupt maps returned by earlier Next calls.
//   - A maximum row limit may be supplied; once the limit is reached (or the
//     result set is exhausted, or Close is called explicitly) the underlying
//     rows are released exactly once.
//   - Two ownership modes are explicitly distinguished: the auto-close mode
//     created by NewMapRows closes the underlying rows on the caller's
//     behalf, while the read-only (caller-held) mode created by
//     NewMapRowsReadOnly never closes them and leaves their lifecycle to the
//     caller.
//   - If scanning a row fails, that scan error is retained as the primary
//     error; any error from the subsequent close is attached to it via a
//     *ScanCloseError so both remain identifiable with errors.Is/errors.As.
package sqlx

import "fmt"

// ScanCloseError combines a primary scan (or iteration) error with a
// secondary error encountered while closing the underlying rows.  The scan
// error is always preserved first; the close error is attached, never
// substituted for it.  Both can be inspected directly through the exported
// fields or via errors.Is/errors.As thanks to Unwrap.
type ScanCloseError struct {
	// Scan is the primary error encountered while scanning or iterating.
	Scan error
	// Close is the error returned when releasing the underlying rows.
	Close error
}

// Error implements the error interface, reporting the scan error first.
func (e *ScanCloseError) Error() string {
	return fmt.Sprintf("maprows: scan error: %v (close error: %v)", e.Scan, e.Close)
}

// Unwrap exposes both the scan error and the close error so that
// errors.Is and errors.As can identify either of them.
func (e *ScanCloseError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.Scan != nil {
		errs = append(errs, e.Scan)
	}
	if e.Close != nil {
		errs = append(errs, e.Close)
	}
	return errs
}

// combineErrors keeps the primary (scan) error first and attaches the close
// error behind it, so neither error can mask the other.
func combineErrors(scanErr, closeErr error) error {
	if scanErr == nil {
		return closeErr
	}
	if closeErr == nil {
		return scanErr
	}
	return &ScanCloseError{Scan: scanErr, Close: closeErr}
}

// MapRows iterates over a result set, returning each row as an independent
// map[string]interface{}.  It does not buffer the whole result set: exactly
// one row is scanned per Next call.
//
// A MapRows must be created with NewMapRows (auto-close mode) or
// NewMapRowsReadOnly (caller-held mode); the zero value is not usable.
type MapRows struct {
	rows rowsi
	cols []string

	limit int // maximum number of rows to yield; <= 0 means no limit
	n     int // number of rows already yielded

	autoClose bool  // true: iterator owns and closes rows; false: caller holds them
	closed    bool  // set once the iterator has finished and released rows
	err       error // sticky terminal error, scan error first then close error
}

// NewMapRows returns a MapRows in auto-close mode.  The iterator takes
// ownership of r and closes it exactly once: when limit rows have been
// yielded, when the result set is exhausted, when a scan error occurs, or
// when Close is called explicitly, whichever happens first.  A limit <= 0
// means no row limit.
func NewMapRows(r rowsi, limit int) *MapRows {
	return &MapRows{rows: r, limit: limit, autoClose: true}
}

// NewMapRowsReadOnly returns a MapRows in caller-held (read-only) mode.
// The iterator never closes r: releasing the underlying rows remains the
// caller's responsibility.  Close merely terminates the iteration.  A
// limit <= 0 means no row limit.
func NewMapRowsReadOnly(r rowsi, limit int) *MapRows {
	return &MapRows{rows: r, limit: limit}
}

// MapRows returns an auto-close MapRows iterator over r with the given
// maximum row limit (<= 0 for no limit).  It does not alter MapScan.
func (r *Rows) MapRows(limit int) *MapRows {
	return NewMapRows(r, limit)
}

// Next scans the next row and returns it as a newly allocated map.  Any
// []byte values are copied, so the returned map is unaffected by driver
// buffer reuse on subsequent Next calls.
//
// Next returns (nil, nil) once the row limit has been reached or the result
// set has been exhausted.  A non-nil error is terminal: the scan error is
// retained first and any close error is attached behind it (see
// ScanCloseError); the same error remains available via Err.
func (m *MapRows) Next() (map[string]interface{}, error) {
	if m.closed {
		return nil, m.err
	}
	if m.limit > 0 && m.n >= m.limit {
		m.finish(nil)
		return nil, m.err
	}
	if !m.rows.Next() {
		// The result set is exhausted; rows.Err() reports any error
		// encountered during iteration and is treated as the primary error.
		m.finish(m.rows.Err())
		return nil, m.err
	}
	row, err := m.scanRow()
	if err != nil {
		m.finish(err)
		return nil, m.err
	}
	m.n++
	if m.limit > 0 && m.n >= m.limit {
		// The limit is reached by this row: release the rows now.  A close
		// error is recorded and surfaced by the next Next/Err call rather
		// than failing this otherwise successful row.
		m.finish(nil)
	}
	return row, nil
}

// Err returns the terminal error of the iterator, if any.  When both a scan
// error and a close error occurred, Err is a *ScanCloseError carrying the
// scan error first and the close error attached.
func (m *MapRows) Err() error {
	return m.err
}

// Close terminates the iteration.  In auto-close mode it releases the
// underlying rows exactly once; in caller-held (read-only) mode it leaves
// the rows untouched.  Close is idempotent: calling it more than once does
// not close the rows again and returns the same result.
func (m *MapRows) Close() error {
	if m.closed {
		return m.err
	}
	m.finish(nil)
	return m.err
}

// finish ends the iteration, releasing the underlying rows exactly once in
// auto-close mode.  scanErr is retained as the primary error and any close
// error is attached behind it.
func (m *MapRows) finish(scanErr error) {
	if m.closed {
		return
	}
	m.closed = true
	var closeErr error
	if m.autoClose {
		closeErr = m.rows.Close()
	}
	m.err = combineErrors(scanErr, closeErr)
}

// columns lazily fetches and caches the column names.
func (m *MapRows) columns() ([]string, error) {
	if m.cols != nil {
		return m.cols, nil
	}
	cols, err := m.rows.Columns()
	if err != nil {
		return nil, err
	}
	m.cols = cols
	return cols, nil
}

// scanRow scans the current row into a fresh map, copying []byte values so
// the map stays valid even if the driver reuses its scan buffers.
func (m *MapRows) scanRow() (map[string]interface{}, error) {
	cols, err := m.columns()
	if err != nil {
		return nil, err
	}
	values := make([]interface{}, len(cols))
	for i := range values {
		values[i] = new(interface{})
	}
	if err := m.rows.Scan(values...); err != nil {
		return nil, err
	}
	row := make(map[string]interface{}, len(cols))
	for i, col := range cols {
		v := *(values[i].(*interface{}))
		if b, ok := v.([]byte); ok && b != nil {
			cp := make([]byte, len(b))
			copy(cp, b)
			v = cp
		}
		row[col] = v
	}
	return row, nil
}
