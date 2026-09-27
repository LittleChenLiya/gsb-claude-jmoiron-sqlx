package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
)

// memDriverName is an in-process database driver used by the group scan
// tests and examples. It never opens a network connection, so the tests do
// not depend on an external database.
const memDriverName = "sqlx-mem"

func init() {
	sql.Register(memDriverName, memDriver{})
}

type memDriver struct{}

func (memDriver) Open(name string) (driver.Conn, error) {
	return &memConn{fixture: memFixtures[name]}, nil
}

// memFixtures maps a DSN-like name to a shared in-memory result set so that
// both *sql.DB.Query and the executable example can reach the same data.
var memFixtures = map[string]*memResult{}

type memConn struct {
	fixture *memResult
}

func (c *memConn) Prepare(query string) (driver.Stmt, error) {
	return &memStmt{fixture: c.fixture}, nil
}

func (c *memConn) Close() error { return nil }

func (c *memConn) Begin() (driver.Tx, error) {
	return nil, errors.New("memDriver: transactions are not supported")
}

func (c *memConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	return (&memStmt{fixture: c.fixture}).Query(args)
}

func (c *memConn) Ping(context.Context) error { return nil }

type memStmt struct {
	fixture *memResult
}

func (s *memStmt) Close() error  { return nil }
func (s *memStmt) NumInput() int { return -1 }
func (s *memStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errors.New("memDriver: Exec is not supported")
}

func (s *memStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.queryRows()
}

// queryContext is selected by database/sql through the optional
// driver.QueryerContext / driver.StmtQueryContext interfaces.
func (s *memStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.queryRows()
}

func (s *memStmt) queryRows() (driver.Rows, error) {
	if s.fixture == nil {
		return nil, errors.New("memDriver: unknown fixture")
	}
	return s.fixture.open(), nil
}

// memResult is one prepared, in-memory query result.
type memResult struct {
	columns []string
	rows    [][]driver.Value
	// failAfterRow, when non-zero, makes Next return failErr immediately
	// after the given number of rows has been consumed. This surfaces as a
	// mid-stream Rows.Err failure through database/sql.
	failAfterRow int
	failErr      error

	closeCount int
}

func (f *memResult) open() *memRows {
	return &memRows{fixture: f}
}

type memRows struct {
	fixture *memResult
	pos     int
	closed  bool
}

func (r *memRows) Columns() []string { return r.fixture.columns }

func (r *memRows) Close() error {
	if !r.closed {
		r.closed = true
		r.fixture.closeCount++
	}
	return nil
}

func (r *memRows) Next(dest []driver.Value) error {
	if r.closed {
		return io.EOF
	}
	if r.fixture.failAfterRow > 0 && r.pos >= r.fixture.failAfterRow {
		return r.fixture.failErr
	}
	if r.pos >= len(r.fixture.rows) {
		return io.EOF
	}
	row := r.fixture.rows[r.pos]
	for i, v := range row {
		dest[i] = v
	}
	r.pos++
	return nil
}

// newMemDB registers fixture under name and returns a *DB connected to it.
func newMemDB(name string, fixture *memResult) *DB {
	memFixtures[name] = fixture
	db, err := Open(memDriverName, name)
	if err != nil {
		panic(err)
	}
	return db
}

// rowsCloseCount reports how many times opened Rows for fixture were closed.
func (f *memResult) rowsCloseCount() int { return f.closeCount }
