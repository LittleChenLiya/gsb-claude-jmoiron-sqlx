package sqlx

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx/reflectx"
)

// Bindvar types supported by Rebind, BindMap and BindStruct.
const (
	UNKNOWN = iota
	QUESTION
	DOLLAR
	NAMED
	AT
)

var defaultBinds = map[int][]string{
	DOLLAR:   {"postgres", "pgx", "pq-timeouts", "cloudsqlpostgres", "ql", "nrpostgres", "cockroach"},
	QUESTION: {"mysql", "sqlite3", "nrmysql", "nrsqlite3"},
	NAMED:    {"oci8", "ora", "goracle", "godror"},
	AT:       {"sqlserver", "azuresql"},
}

var binds sync.Map

func init() {
	for bind, drivers := range defaultBinds {
		for _, driver := range drivers {
			BindDriver(driver, bind)
		}
	}

}

// BindType returns the bindtype for a given database given a drivername.
func BindType(driverName string) int {
	itype, ok := binds.Load(driverName)
	if !ok {
		return UNKNOWN
	}
	return itype.(int)
}

// BindDriver sets the BindType for driverName to bindType.
func BindDriver(driverName string, bindType int) {
	binds.Store(driverName, bindType)
}

// FIXME: this should be able to be tolerant of escaped ?'s in queries without
// losing much speed, and should be to avoid confusion.

// Rebind a query from the default bindtype (QUESTION) to the target bindtype.
func Rebind(bindType int, query string) string {
	switch bindType {
	case QUESTION, UNKNOWN:
		return query
	}

	// Add space enough for 10 params before we have to allocate
	rqb := make([]byte, 0, len(query)+10)

	var i, j int

	for i = strings.Index(query, "?"); i != -1; i = strings.Index(query, "?") {
		rqb = append(rqb, query[:i]...)

		switch bindType {
		case DOLLAR:
			rqb = append(rqb, '$')
		case NAMED:
			rqb = append(rqb, ':', 'a', 'r', 'g')
		case AT:
			rqb = append(rqb, '@', 'p')
		}

		j++
		rqb = strconv.AppendInt(rqb, int64(j), 10)

		query = query[i+1:]
	}

	return string(append(rqb, query...))
}

// Experimental implementation of Rebind which uses a bytes.Buffer.  The code is
// much simpler and should be more resistant to odd unicode, but it is twice as
// slow.  Kept here for benchmarking purposes and to possibly replace Rebind if
// problems arise with its somewhat naive handling of unicode.
func rebindBuff(bindType int, query string) string {
	if bindType != DOLLAR {
		return query
	}

	b := make([]byte, 0, len(query))
	rqb := bytes.NewBuffer(b)
	j := 1
	for _, r := range query {
		if r == '?' {
			rqb.WriteRune('$')
			rqb.WriteString(strconv.Itoa(j))
			j++
		} else {
			rqb.WriteRune(r)
		}
	}

	return rqb.String()
}

func asSliceForIn(i interface{}) (v reflect.Value, ok bool) {
	if i == nil {
		return reflect.Value{}, false
	}

	v = reflect.ValueOf(i)
	t := reflectx.Deref(v.Type())

	// Only expand slices
	if t.Kind() != reflect.Slice {
		return reflect.Value{}, false
	}

	// []byte is a driver.Value type so it should not be expanded
	if t == reflect.TypeOf([]byte{}) {
		return reflect.Value{}, false

	}

	return v, true
}

// In expands slice values in args, returning the modified query string
// and a new arg list that can be executed by a database. The `query` should
// use the `?` bindVar.  The return value uses the `?` bindVar.
func In(query string, args ...interface{}) (string, []interface{}, error) {
	// argMeta stores reflect.Value and length for slices and
	// the value itself for non-slice arguments
	type argMeta struct {
		v      reflect.Value
		i      interface{}
		length int
	}

	var flatArgsCount int
	var anySlices bool

	var stackMeta [32]argMeta

	var meta []argMeta
	if len(args) <= len(stackMeta) {
		meta = stackMeta[:len(args)]
	} else {
		meta = make([]argMeta, len(args))
	}

	for i, arg := range args {
		if a, ok := arg.(driver.Valuer); ok {
			var err error
			arg, err = a.Value()
			if err != nil {
				return "", nil, err
			}
		}

		if v, ok := asSliceForIn(arg); ok {
			meta[i].length = v.Len()
			meta[i].v = v

			anySlices = true
			flatArgsCount += meta[i].length

			if meta[i].length == 0 {
				return "", nil, errors.New("empty slice passed to 'in' query")
			}
		} else {
			meta[i].i = arg
			flatArgsCount++
		}
	}

	// don't do any parsing if there aren't any slices;  note that this means
	// some errors that we might have caught below will not be returned.
	if !anySlices {
		return query, args, nil
	}

	newArgs := make([]interface{}, 0, flatArgsCount)

	var buf strings.Builder
	buf.Grow(len(query) + len(", ?")*flatArgsCount)

	var arg, offset int

	for i := strings.IndexByte(query[offset:], '?'); i != -1; i = strings.IndexByte(query[offset:], '?') {
		if arg >= len(meta) {
			// if an argument wasn't passed, lets return an error;  this is
			// not actually how database/sql Exec/Query works, but since we are
			// creating an argument list programmatically, we want to be able
			// to catch these programmer errors earlier.
			return "", nil, errors.New("number of bindVars exceeds arguments")
		}

		argMeta := meta[arg]
		arg++

		// not a slice, continue.
		// our questionmark will either be written before the next expansion
		// of a slice or after the loop when writing the rest of the query
		if argMeta.length == 0 {
			offset = offset + i + 1
			newArgs = append(newArgs, argMeta.i)
			continue
		}

		// write everything up to and including our ? character
		buf.WriteString(query[:offset+i+1])

		for si := 1; si < argMeta.length; si++ {
			buf.WriteString(", ?")
		}

		newArgs = appendReflectSlice(newArgs, argMeta.v, argMeta.length)

		// slice the query and reset the offset. this avoids some bookkeeping for
		// the write after the loop
		query = query[offset+i+1:]
		offset = 0
	}

	buf.WriteString(query)

	if arg < len(meta) {
		return "", nil, errors.New("number of bindVars less than number arguments")
	}

	return buf.String(), newArgs, nil
}

// InEmptyStrategy determines how InCon handles empty slice arguments.
// The zero value (InEmptyReject) preserves the legacy In behavior.
type InEmptyStrategy int

const (
	// InEmptyReject is the zero value and matches the legacy In behavior:
	// an empty slice argument causes an error and no query is produced.
	InEmptyReject InEmptyStrategy = iota
	// InEmptyNull replaces the placeholder of an empty slice argument with
	// the SQL NULL keyword and contributes no bind values, so a query like
	// `WHERE id IN (?)` becomes `WHERE id IN (NULL)`.
	InEmptyNull
)

// InOptions configures the behavior of InCon.
type InOptions struct {
	// Empty controls how empty slice arguments are handled.
	Empty InEmptyStrategy
}

// InCon is a configurable version of In.  It expands slice values in args,
// returning the modified query string and a new arg list that can be executed
// by a database.  The query should use the `?` bindVar; the return value uses
// the `?` bindVar as well.
//
// Unlike In, InCon always validates the query and arguments before producing
// any output: the number of `?` placeholders (ignoring any `?` inside quoted
// strings or identifiers) must match the number of arguments, and every empty
// slice argument is resolved according to opts.Empty before a new query is
// built.  When validation fails the returned query is empty and the arg list
// is nil, so callers never observe a partially rewritten query; the input
// args slice is never modified.
//
// Only genuine slice values are expanded: []byte stays a single value, and a
// driver.Valuer is consulted exactly once to determine whether its value is a
// slice.
func InCon(query string, args []interface{}, opts InOptions) (string, []interface{}, error) {
	// argMeta stores reflect.Value and length for slices and
	// the value itself for non-slice arguments.
	type argMeta struct {
		v      reflect.Value
		i      interface{}
		length int
	}

	meta := make([]argMeta, len(args))
	flatArgsCount := 0

	// Pass 1: validate every argument and the placeholder count before
	// producing any output.  A failure here returns an empty query and a nil
	// arg list so callers never observe a partially rewritten query, and the
	// input args slice is left untouched.
	for i, arg := range args {
		// A driver.Valuer is consulted exactly once; the returned value is
		// what gets classified (and possibly expanded) below.  It is never
		// called again to detect whether it is a slice.
		if a, ok := arg.(driver.Valuer); ok {
			var err error
			arg, err = a.Value()
			if err != nil {
				return "", nil, fmt.Errorf("InCon: argument %d: %w", i, err)
			}
		}

		if v, ok := asSliceForIn(arg); ok {
			meta[i].length = v.Len()
			meta[i].v = v
			flatArgsCount += meta[i].length

			if meta[i].length == 0 {
				switch opts.Empty {
				case InEmptyNull:
					// NULL strategy: the placeholder is replaced by NULL and
					// no bind values are contributed.
				case InEmptyReject:
					return "", nil, fmt.Errorf("InCon: empty slice passed to 'in' query at argument %d", i)
				default:
					return "", nil, fmt.Errorf("InCon: unknown empty-slice strategy %d", opts.Empty)
				}
			}
		} else {
			meta[i].i = arg
			flatArgsCount++
		}
	}

	// Count placeholders (ignoring '?' inside quoted strings/identifiers)
	// and make sure they match the number of arguments.
	bindVarCount := 0
	for pos := nextPlaceholder(query, 0); pos != -1; pos = nextPlaceholder(query, pos+1) {
		bindVarCount++
	}
	if bindVarCount != len(args) {
		return "", nil, fmt.Errorf("InCon: number of bindVars (%d) does not match number of arguments (%d)", bindVarCount, len(args))
	}

	// Pass 2: build the new query and argument list.
	newArgs := make([]interface{}, 0, flatArgsCount)

	var buf strings.Builder
	buf.Grow(len(query) + len(", ?")*flatArgsCount)

	var argIdx int
	offset := 0
	for pos := nextPlaceholder(query, offset); pos != -1; pos = nextPlaceholder(query, offset) {
		m := meta[argIdx]
		argIdx++

		// Write everything before the placeholder.
		buf.WriteString(query[offset:pos])

		switch {
		case m.length > 0:
			// Slice: expand into one placeholder per element.
			buf.WriteByte('?')
			for si := 1; si < m.length; si++ {
				buf.WriteString(", ?")
			}
			newArgs = appendReflectSlice(newArgs, m.v, m.length)
		case m.v.IsValid():
			// Empty slice with the NULL strategy: a single SQL NULL, no args.
			buf.WriteString("NULL")
		default:
			// Scalar value.
			buf.WriteByte('?')
			newArgs = append(newArgs, m.i)
		}

		offset = pos + 1
	}
	buf.WriteString(query[offset:])

	return buf.String(), newArgs, nil
}

// nextPlaceholder returns the index of the next '?' placeholder at or after
// start that is not inside a quoted string literal or quoted identifier.
// Single-quoted strings, double-quoted strings/identifiers and backtick
// identifiers are all recognized; a doubled quote escapes itself, and a
// backslash escapes the following character (MySQL style).
func nextPlaceholder(query string, start int) int {
	var quote byte // 0 means unquoted
	for i := start; i < len(query); i++ {
		c := query[i]
		if quote != 0 {
			if c == '\\' && i+1 < len(query) {
				// Backslash escapes the next character (e.g. '\'' in MySQL).
				i++
				continue
			}
			if c == quote {
				if i+1 < len(query) && query[i+1] == quote {
					// Doubled quote is an escaped quote (e.g. 'it''s').
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '?':
			return i
		}
	}
	return -1
}

func appendReflectSlice(args []interface{}, v reflect.Value, vlen int) []interface{} {
	switch val := v.Interface().(type) {
	case []interface{}:
		args = append(args, val...)
	case []int:
		for i := range val {
			args = append(args, val[i])
		}
	case []string:
		for i := range val {
			args = append(args, val[i])
		}
	default:
		for si := 0; si < vlen; si++ {
			args = append(args, v.Index(si).Interface())
		}
	}

	return args
}
