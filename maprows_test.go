package sqlx

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"
)

const mapRowsFakeDriverName = "maprows_fake"

type mapRowsFakeDriver struct {
	mu         sync.Mutex
	closeCount int
	scanFail   bool
	closeFail  bool
	closeErr   error
}

func (d *mapRowsFakeDriver) Open(name string) (driver.Conn, error) {
	return &mapRowsFakeConn{driver: d}, nil
}

type mapRowsFakeConn struct{ driver *mapRowsFakeDriver }

func (c *mapRowsFakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("maprows: prepare not supported")
}
func (c *mapRowsFakeConn) Close() error { return nil }
func (c *mapRowsFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("maprows: begin not supported")
}
func (c *mapRowsFakeConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	return &mapRowsFakeRows{driver: c.driver, rows: [][]byte{[]byte("alpha"), []byte("beta")}}, nil
}

var (
	errMapRowsScan  = errors.New("maprows: scan failed")
	errMapRowsClose = errors.New("maprows: close failed")
)

type mapRowsFakeRows struct {
	driver *mapRowsFakeDriver
	rows   [][]byte
	idx    int
	buf    []byte
	closed bool
}

func (r *mapRowsFakeRows) Columns() []string { return []string{"id", "payload"} }

func (r *mapRowsFakeRows) Close() error {
	r.driver.mu.Lock()
	defer r.driver.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.driver.closeCount++
	if r.driver.closeFail {
		r.driver.closeErr = errMapRowsClose
		return errMapRowsClose
	}
	return nil
}

func (r *mapRowsFakeRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	if r.driver.scanFail && r.idx == 1 {
		return errMapRowsScan
	}
	if r.buf == nil {
		r.buf = make([]byte, 0, 16)
	}
	r.buf = append(r.buf[:0], r.rows[r.idx]...)
	dest[0] = int64(r.idx + 1)
	dest[1] = r.buf
	r.idx++
	return nil
}

func newMapRowsFakeDB(t *testing.T, scanFail, closeFail bool) (*sql.DB, *mapRowsFakeDriver) {
	t.Helper()
	d := &mapRowsFakeDriver{scanFail: scanFail, closeFail: closeFail}
	name := mapRowsFakeDriverName + "_" + t.Name()
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, d
}

func queryMapRows(t *testing.T, db *sql.DB) *Rows {
	t.Helper()
	xdb := NewDb(db, "maprows_fake")
	rows, err := xdb.Queryx("SELECT id, payload FROM stuff")
	if err != nil {
		t.Fatalf("Queryx: %v", err)
	}
	return rows
}

func payloadString(m map[string]interface{}) string {
	switch v := m["payload"].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

func TestMapRowsBufferReuse(t *testing.T) {
	db, _ := newMapRowsFakeDB(t, false, false)
	rows := queryMapRows(t, db)
	it := rows.MapRows(0, MapRowsHold)
	defer it.Close()
	if !it.Next() {
		t.Fatal("expected first row")
	}
	first := it.Map()
	if got := payloadString(first); got != "alpha" {
		t.Fatalf("first payload = %q, want alpha", got)
	}
	if !it.Next() {
		t.Fatal("expected second row")
	}
	second := it.Map()
	if got := payloadString(second); got != "beta" {
		t.Fatalf("second payload = %q, want beta", got)
	}
	if got := payloadString(first); got != "alpha" {
		t.Errorf("first payload after second scan = %q, want alpha (buffer was reused)", got)
	}
	if first["id"] != int64(1) || second["id"] != int64(2) {
		t.Errorf("ids = %v, %v; want 1, 2", first["id"], second["id"])
	}
}

func TestMapRowsLimitAutoClose(t *testing.T) {
	db, d := newMapRowsFakeDB(t, false, false)
	rows := queryMapRows(t, db)
	it := rows.MapRows(1, MapRowsAutoClose)
	defer it.Close()
	if !it.Next() {
		t.Fatal("expected first row")
	}
	if it.Next() {
		t.Fatal("expected Next to return false after limit")
	}
	d.mu.Lock()
	count := d.closeCount
	d.mu.Unlock()
	if count != 1 {
		t.Errorf("closeCount = %d, want 1 (rows must be released once at limit)", count)
	}
	if err := it.Close(); err != nil {
		t.Errorf("Close after limit = %v, want nil", err)
	}
	d.mu.Lock()
	count = d.closeCount
	d.mu.Unlock()
	if count != 1 {
		t.Errorf("closeCount after Close = %d, want 1 (idempotent)", count)
	}
}

func TestMapRowsScanAndCloseErrors(t *testing.T) {
	db, d := newMapRowsFakeDB(t, true, true)
	rows := queryMapRows(t, db)
	it := rows.MapRows(10, MapRowsAutoClose)
	defer it.Close()
	if !it.Next() {
		t.Fatal("expected first row")
	}
	if it.Next() {
		t.Fatal("expected scan failure on second row")
	}
	err := it.Close()
	if err == nil {
		t.Fatal("expected combined error, got nil")
	}
	if !errors.Is(err, errMapRowsScan) {
		t.Errorf("error %v does not wrap scan error %v", err, errMapRowsScan)
	}
	// When both errors are present the iterator returns *mapRowsError; when
	// database/sql discards the close error (Go 1.26 closes rows on Next
	// failure) the scan error is returned directly. Either way the scan error
	// is identifiable and the close error is kept by the driver.
	var ce *mapRowsError
	if errors.As(err, &ce) {
		if ce.scanErr != errMapRowsScan {
			t.Errorf("scanErr = %v, want %v", ce.scanErr, errMapRowsScan)
		}
	}
	d.mu.Lock()
	closeErr := d.closeErr
	d.mu.Unlock()
	if closeErr != errMapRowsClose {
		t.Errorf("closeErr = %v, want %v (close error must be identifiable)", closeErr, errMapRowsClose)
	}
}

func TestMapRowsReadOnlyMode(t *testing.T) {
	db, d := newMapRowsFakeDB(t, false, false)
	rows := queryMapRows(t, db)
	it := rows.MapRows(1, MapRowsHold)
	defer it.Close()
	if !it.Next() {
		t.Fatal("expected first row")
	}
	first := it.Map()
	if got := payloadString(first); got != "alpha" {
		t.Fatalf("first payload = %q, want alpha", got)
	}
	if it.Next() {
		t.Fatal("expected Next to return false after limit")
	}
	d.mu.Lock()
	count := d.closeCount
	d.mu.Unlock()
	if count != 0 {
		t.Errorf("closeCount after limit = %d, want 0 (read-only mode must not release rows)", count)
	}
	if err := it.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	d.mu.Lock()
	count = d.closeCount
	d.mu.Unlock()
	if count != 1 {
		t.Errorf("closeCount after Close = %d, want 1", count)
	}
}

func TestMapRowsEarlyExit(t *testing.T) {
	db, d := newMapRowsFakeDB(t, false, false)
	rows := queryMapRows(t, db)
	it := rows.MapRows(10, MapRowsAutoClose)
	defer it.Close()
	if !it.Next() {
		t.Fatal("expected first row")
	}
	first := it.Map()
	if got := payloadString(first); got != "alpha" {
		t.Fatalf("first payload = %q, want alpha", got)
	}
	if err := it.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	d.mu.Lock()
	count := d.closeCount
	d.mu.Unlock()
	if count != 1 {
		t.Errorf("closeCount after early exit = %d, want 1", count)
	}
}

func TestMapRowsRepeatedClose(t *testing.T) {
	db, d := newMapRowsFakeDB(t, false, false)
	rows := queryMapRows(t, db)
	it := rows.MapRows(1, MapRowsAutoClose)
	defer it.Close()
	if !it.Next() {
		t.Fatal("expected first row")
	}
	if it.Next() {
		t.Fatal("expected Next to return false after limit")
	}
	for i := 0; i < 3; i++ {
		if err := it.Close(); err != nil {
			t.Errorf("Close #%d = %v, want nil", i+1, err)
		}
	}
	d.mu.Lock()
	count := d.closeCount
	d.mu.Unlock()
	if count != 1 {
		t.Errorf("closeCount after repeated Close = %d, want 1", count)
	}
}

func TestMapRowsMapReturnsFreshMap(t *testing.T) {
	db, _ := newMapRowsFakeDB(t, false, false)
	rows := queryMapRows(t, db)
	it := rows.MapRows(0, MapRowsHold)
	defer it.Close()
	if !it.Next() {
		t.Fatal("expected first row")
	}
	m1 := it.Map()
	if !it.Next() {
		t.Fatal("expected second row")
	}
	m2 := it.Map()
	if len(m1) != 2 || len(m2) != 2 {
		t.Errorf("map lengths = %d, %d; want 2, 2", len(m1), len(m2))
	}
	if &m1 == &m2 {
		t.Error("Map returned the same map twice")
	}
	if payloadString(m1) != "alpha" || payloadString(m2) != "beta" {
		t.Errorf("payloads = %q, %q; want alpha, beta", payloadString(m1), payloadString(m2))
	}
}

// ExampleMapRows demonstrates a short iteration over a result set using the
// auto-close mode with a row limit.
func ExampleMapRows() {
	var once sync.Once
	once.Do(func() { sql.Register("maprows_example", &mapRowsFakeDriver{}) })
	db, err := sql.Open("maprows_example", "")
	if err != nil {
		return
	}
	defer db.Close()

	xdb := NewDb(db, "maprows_example")
	rows, err := xdb.Queryx("SELECT id, payload FROM stuff")
	if err != nil {
		return
	}

	it := rows.MapRows(10, MapRowsAutoClose)
	defer it.Close()

	for it.Next() {
		m := it.Map()
		_ = m["id"]
		_ = m["payload"]
	}
}
