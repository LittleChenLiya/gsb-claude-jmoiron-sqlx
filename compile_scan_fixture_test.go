package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// memDriverName is a tiny, pure-Go database/sql driver that keeps everything
// in memory. It exists only for the CompileStructScan tests and examples, so
// that no external database, CGO driver or container is needed.
//
// Query format:
//
//	mem:<col1>,<col2>,...|<v11>:<v12>:...|<v21>:...
//
// Cells are separated by ':'; rows by '|'. Cell value prefixes:
//
//	i:<n>  int64 (a bare "<n>" without a prefix is also parsed as int64)
//	s:<s>  string
//	n:     NULL
//	b:<s>  []byte of the given text
//	x:     a value that fails conversion into int64/string scan targets,
//	       used to exercise the "conversion failed" path
//
// For example: "mem:id,name|1:alice|2:bob".
const memDriverName = "sqlx-mem"

type memDriver struct{}

func (memDriver) Open(name string) (driver.Conn, error) {
	return &memConn{name: name}, nil
}

type memConn struct {
	name string
}

func (c *memConn) Prepare(query string) (driver.Stmt, error) {
	return &memStmt{query: query}, nil
}
func (c *memConn) Close() error { return nil }
func (c *memConn) Begin() (driver.Tx, error) {
	return nil, errors.New("mem: transactions are not supported")
}
func (c *memConn) Ping(context.Context) error { return nil }

type memStmt struct {
	query string
}

func (s *memStmt) Close() error  { return nil }
func (s *memStmt) NumInput() int { return -1 }

func (s *memStmt) Exec(args []driver.Value) (driver.Result, error) {
	return memResult{}, nil
}

func (s *memStmt) Query(args []driver.Value) (driver.Rows, error) {
	return parseMemQuery(s.query)
}

type memResult struct{}

func (memResult) LastInsertId() (int64, error) { return 0, nil }
func (memResult) RowsAffected() (int64, error) { return 0, nil }

type memRows struct {
	columns []string
	rows    [][]driver.Value
	pos     int
}

func (r *memRows) Columns() []string { return r.columns }

func (r *memRows) Close() error {
	r.pos = len(r.rows)
	return nil
}

func (r *memRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

// badValue cannot be converted to int or string targets by database/sql's
// convertAssign: it is a struct, which forces a conversion error.
type badValue struct{}

func parseMemQuery(query string) (*memRows, error) {
	if !strings.HasPrefix(query, "mem:") {
		return nil, fmt.Errorf("mem: query must start with 'mem:', got %q", query)
	}
	body := strings.TrimPrefix(query, "mem:")
	parts := strings.Split(body, "|")
	rows := &memRows{columns: strings.Split(parts[0], ",")}
	for _, raw := range parts[1:] {
		if raw == "" {
			continue
		}
		cells := strings.Split(raw, ":")
		if len(cells) != len(rows.columns) {
			return nil, fmt.Errorf("mem: row %q has %d cell(s), want %d", raw, len(cells), len(rows.columns))
		}
		vals := make([]driver.Value, len(cells))
		for i, cell := range cells {
			vals[i] = parseMemCell(cell)
		}
		rows.rows = append(rows.rows, vals)
	}
	return rows, nil
}

func parseMemCell(cell string) driver.Value {
	switch {
	case cell == "n" || cell == "n:":
		return nil
	case cell == "x" || cell == "x:":
		return badValue{}
	case strings.HasPrefix(cell, "s:"):
		return strings.TrimPrefix(cell, "s:")
	case strings.HasPrefix(cell, "b:"):
		return []byte(strings.TrimPrefix(cell, "b:"))
	case strings.HasPrefix(cell, "i:"):
		n, err := strconv.ParseInt(strings.TrimPrefix(cell, "i:"), 10, 64)
		if err != nil {
			return cell
		}
		return n
	default:
		if n, err := strconv.ParseInt(cell, 10, 64); err == nil {
			return n
		}
		return cell
	}
}

func init() {
	sql.Register(memDriverName, memDriver{})
}

// memOpen opens an sqlx connection backed by the in-memory driver.
func memOpen(t interface{ Fatal(...interface{}) }) *DB {
	db, err := Open(memDriverName, "")
	if err != nil {
		t.Fatal(err)
	}
	return db
}
