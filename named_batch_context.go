//go:build go1.8
// +build go1.8

package sqlx

// Batched Named Exec Support
//
// NamedExecBatchesContext executes a named INSERT template against a slice of
// structs or maps, splitting the rows into batches that respect a maximum
// parameter budget.  Unlike bindArray/fixBound, the VALUES tuple is located
// with a bracket-aware scanner that skips SQL comments and string literals,
// so commas or VALUES keywords inside them are never mistaken for syntax.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode"

	"github.com/jmoiron/sqlx/reflectx"
)

// segments of a named INSERT template, used to decide how often each named
// parameter contributes to the parameter budget of a batch
const (
	namedSegHead   = iota // before the VALUES tuple: bound once per batch
	namedSegValues        // inside the VALUES tuple: bound once per row
	namedSegTail          // after the VALUES tuple: bound once per batch
)

// skipLiteral returns the index just past the SQL comment or quoted literal
// starting at qs[i], or -1 if qs[i] does not start one.  It understands
// -- line comments, block comments, single quoted strings (with doubled
// quote and backslash escapes), double quoted identifiers and backticks.
func skipLiteral(qs []byte, i int) int {
	switch qs[i] {
	case '-':
		if i+1 < len(qs) && qs[i+1] == '-' {
			j := i + 2
			for j < len(qs) && qs[j] != '\n' {
				j++
			}
			return j
		}
	case '/':
		if i+1 < len(qs) && qs[i+1] == '*' {
			j := i + 2
			for j+1 < len(qs) && !(qs[j] == '*' && qs[j+1] == '/') {
				j++
			}
			if j+1 < len(qs) {
				return j + 2
			}
			return len(qs)
		}
	case '\'':
		j := i + 1
		for j < len(qs) {
			if qs[j] == '\\' && j+1 < len(qs) {
				j += 2
				continue
			}
			if qs[j] == '\'' {
				if j+1 < len(qs) && qs[j+1] == '\'' {
					j += 2
					continue
				}
				return j + 1
			}
			j++
		}
		return len(qs)
	case '"':
		j := i + 1
		for j < len(qs) {
			if qs[j] == '"' {
				if j+1 < len(qs) && qs[j+1] == '"' {
					j += 2
					continue
				}
				return j + 1
			}
			j++
		}
		return len(qs)
	case 0x60: // backtick
		j := i + 1
		for j < len(qs) && qs[j] != 0x60 {
			j++
		}
		if j < len(qs) {
			return j + 1
		}
		return len(qs)
	}
	return -1
}

func isIdentByte(b byte) bool {
	return b == '_' || unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b))
}

// findValuesTuple locates the VALUES keyword of an INSERT statement (skipping
// comments and string literals) and returns the indices of the opening and
// closing brackets of the first tuple following it.
func findValuesTuple(qs []byte) (int, int, error) {
	for i := 0; i < len(qs); i++ {
		if j := skipLiteral(qs, i); j >= 0 {
			i = j - 1
			continue
		}
		if len(qs)-i >= 6 && strings.EqualFold(string(qs[i:i+6]), "VALUES") &&
			(i == 0 || !isIdentByte(qs[i-1])) &&
			(i+6 == len(qs) || !isIdentByte(qs[i+6])) {
			for j := i + 6; j < len(qs); j++ {
				if k := skipLiteral(qs, j); k >= 0 {
					j = k - 1
					continue
				}
				if qs[j] != '(' {
					continue
				}
				depth := 0
				for k := j; k < len(qs); k++ {
					if l := skipLiteral(qs, k); l >= 0 {
						k = l - 1
						continue
					}
					switch qs[k] {
					case '(':
						depth++
					case ')':
						depth--
						if depth == 0 {
							return j, k, nil
						}
					}
				}
				return 0, 0, errors.New("sqlx: unbalanced brackets in VALUES tuple")
			}
			return 0, 0, errors.New("sqlx: no tuple found after VALUES")
		}
	}
	return 0, 0, errors.New("sqlx: query does not contain a VALUES clause")
}

func namedSegment(pos, open, close int) int {
	switch {
	case pos > open && pos < close:
		return namedSegValues
	case pos < open:
		return namedSegHead
	default:
		return namedSegTail
	}
}

// compileNamedInsert compiles a named INSERT template into a query using the
// '?' bindvar, a list of parameter names and the template segment each name
// belongs to.  Comments and string literals are copied verbatim and never
// scanned for named parameters.
func compileNamedInsert(qs []byte) (query string, names []string, segments []int, err error) {
	open, close, err := findValuesTuple(qs)
	if err != nil {
		return "", nil, nil, err
	}

	names = make([]string, 0, 10)
	segments = make([]int, 0, 10)
	rebound := make([]byte, 0, len(qs))

	inName := false
	nameStart := 0
	last := len(qs) - 1
	name := make([]byte, 0, 10)

	for i := 0; i < len(qs); i++ {
		b := qs[i]

		if !inName {
			if j := skipLiteral(qs, i); j >= 0 {
				rebound = append(rebound, qs[i:j]...)
				i = j - 1
				continue
			}
		}

		// a ':' while we're in a name is an error
		if b == ':' {
			// if this is the second ':' in a '::' escape sequence, append a ':'
			if inName && i > 0 && qs[i-1] == ':' {
				rebound = append(rebound, ':')
				inName = false
				continue
			} else if inName {
				err = errors.New("unexpected ':' while reading named param at " + strconv.Itoa(i))
				return query, names, segments, err
			}
			inName = true
			nameStart = i
			name = []byte{}
		} else if inName && i > 0 && b == '=' && len(name) == 0 {
			rebound = append(rebound, ':', '=')
			inName = false
			continue
			// if we're in a name, and this is an allowed character, continue
		} else if inName && (unicode.IsOneOf(allowedBindRunes, rune(b)) || b == '_' || b == '.') && i != last {
			// append the byte to the name if we are in a name and not on the last byte
			name = append(name, b)
			// if we're in a name and it's not an allowed character, the name is done
		} else if inName {
			inName = false
			// if this is the final byte of the string and it is part of the name, then
			// make sure to add it to the name
			if i == last && unicode.IsOneOf(allowedBindRunes, rune(b)) {
				name = append(name, b)
			}
			// add the string representation to the names list
			names = append(names, string(name))
			segments = append(segments, namedSegment(nameStart, open, close))
			// add a bindvar;  the batch builder rebinds to the driver bindtype
			rebound = append(rebound, '?')
			// add this byte to string unless it was not part of the name
			if i != last {
				rebound = append(rebound, b)
			} else if !unicode.IsOneOf(allowedBindRunes, rune(b)) {
				rebound = append(rebound, b)
			}
		} else {
			// this is a normal byte and should just go onto the rebound query
			rebound = append(rebound, b)
		}
	}

	return string(rebound), names, segments, err
}

// namedBatch is a single executable batch: a rebound query and its arguments.
type namedBatch struct {
	query string
	args  []interface{}
}

// bindNamedBatches binds a named INSERT template with a slice of structs or
// maps, splitting the rows into batches of at most maxParams bind parameters
// (counting repeated named parameters inside the VALUES tuple once per row
// and parameters outside the tuple once per batch).  A maxParams <= 0 means
// no limit.  Every row is bound before the first query is built, so a binding
// error on any row yields zero batches.
func bindNamedBatches(bindType int, query string, arg interface{}, m *reflectx.Mapper, maxParams int) ([]namedBatch, error) {
	bound, names, segments, err := compileNamedInsert([]byte(query))
	if err != nil {
		return nil, err
	}
	// locate the VALUES tuple in the compiled query;  the compilation only
	// replaces ':name' with '?' outside of comments and literals, so the
	// tuple is found at the same place
	tupleOpen, tupleClose, err := findValuesTuple([]byte(bound))
	if err != nil {
		return nil, err
	}

	v := reflect.ValueOf(arg)
	for v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if k := v.Kind(); k != reflect.Slice && k != reflect.Array {
		return nil, fmt.Errorf("sqlx: expected a slice or array of structs or maps, got %T", arg)
	}
	arrayLen := v.Len()
	if arrayLen == 0 {
		return nil, fmt.Errorf("length of array is 0: %#v", arg)
	}

	// bind every row up front so that a missing field on any row fails
	// before a single statement is sent
	rowArgs := make([][]interface{}, arrayLen)
	for i := 0; i < arrayLen; i++ {
		rowArgs[i], err = bindAnyArgs(names, v.Index(i).Interface(), m)
		if err != nil {
			return nil, fmt.Errorf("sqlx: could not bind row %d: %v", i, err)
		}
	}

	var perRow, fixed int
	var headIdx, valuesIdx, tailIdx []int
	for i, s := range segments {
		switch s {
		case namedSegValues:
			perRow++
			valuesIdx = append(valuesIdx, i)
		case namedSegHead:
			fixed++
			headIdx = append(headIdx, i)
		default:
			fixed++
			tailIdx = append(tailIdx, i)
		}
	}
	if perRow == 0 {
		return nil, errors.New("sqlx: no named parameters found inside the VALUES tuple")
	}

	rowsPerBatch := arrayLen
	if maxParams > 0 {
		rowsPerBatch = (maxParams - fixed) / perRow
		if rowsPerBatch < 1 {
			return nil, fmt.Errorf("sqlx: maxParams %d is too small for %d fixed and %d per-row parameters", maxParams, fixed, perRow)
		}
		if rowsPerBatch > arrayLen {
			rowsPerBatch = arrayLen
		}
	}

	head := bound[:tupleOpen]
	tuple := bound[tupleOpen : tupleClose+1]
	tail := bound[tupleClose+1:]

	var batches []namedBatch
	for start := 0; start < arrayLen; start += rowsPerBatch {
		end := start + rowsPerBatch
		if end > arrayLen {
			end = arrayLen
		}

		var buf bytes.Buffer
		buf.WriteString(head)
		args := make([]interface{}, 0, fixed+perRow*(end-start))
		for _, idx := range headIdx {
			args = append(args, rowArgs[start][idx])
		}
		for i := start; i < end; i++ {
			if i > start {
				buf.WriteString(", ")
			}
			buf.WriteString(tuple)
			for _, idx := range valuesIdx {
				args = append(args, rowArgs[i][idx])
			}
		}
		buf.WriteString(tail)
		for _, idx := range tailIdx {
			args = append(args, rowArgs[start][idx])
		}

		q := buf.String()
		// rebind each batch separately so numbered bindvars (e.g. $1, @p1)
		// restart from 1 in every batch
		if bindType != QUESTION {
			q = Rebind(bindType, q)
		}
		batches = append(batches, namedBatch{query: q, args: args})
	}
	return batches, nil
}

// NamedExecBatchesContext executes a named INSERT template against arg, a
// slice or array of structs or maps, splitting the rows into batches that
// stay within maxParams bind parameters per statement (a value <= 0 disables
// batching).  The placeholder style is derived from the driver of e, and
// numbered placeholders restart from 1 in every batch.
//
// All rows are bound before the first statement is executed, so a row that
// cannot be bound results in zero statements being sent.  The batches are
// executed on e itself, so when e is a *Tx the whole batch runs inside the
// caller's transaction;  on error the successfully executed batch results are
// returned alongside the error and it is up to the caller to roll back.
func NamedExecBatchesContext(ctx context.Context, e ExtContext, query string, arg interface{}, maxParams int) ([]sql.Result, error) {
	batches, err := bindNamedBatches(BindType(e.DriverName()), query, arg, mapperFor(e), maxParams)
	if err != nil {
		return nil, err
	}
	results := make([]sql.Result, 0, len(batches))
	for _, b := range batches {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		res, err := e.ExecContext(ctx, b.query, b.args...)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}
