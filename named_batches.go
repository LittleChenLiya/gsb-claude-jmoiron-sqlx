package sqlx

// Named INSERT batching support.
//
// bindNamedBatches compiles a named INSERT ... VALUES (...) template and a
// slice of struct or map rows into a list of executable statements that stay
// within a driver bind parameter budget.  NamedExecBatchesContext (in
// named_context.go) executes those statements.

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"unicode"

	"github.com/jmoiron/sqlx/reflectx"
)

// namedTemplatePart is a segment of a parsed named INSERT template:  either a
// literal chunk of SQL or a single named bind parameter.
type namedTemplatePart struct {
	literal string // literal SQL text;  empty for bind parameters
	name    string // bind parameter name;  empty for literals
	inTuple bool   // part of the VALUES (...) tuple repeated for every row
}

// namedInsertTemplate is a parsed INSERT ... VALUES (...) template that uses
// named bind parameters.  The VALUES tuple is kept apart from the rest of the
// statement so it can be repeated once per row when a batch is assembled.
type namedInsertTemplate struct {
	prefixParts []namedTemplatePart
	tupleParts  []namedTemplatePart
	suffixParts []namedTemplatePart

	prefixNames []string // bind parameters before the VALUES tuple
	rowNames    []string // bind parameters inside the VALUES tuple, per row
	suffixNames []string // bind parameters after the VALUES tuple
}

func isBindRune(b byte) bool {
	return unicode.IsOneOf(allowedBindRunes, rune(b)) || b == '_' || b == '.'
}

func isIdentRune(b byte) bool {
	return unicode.IsOneOf(allowedBindRunes, rune(b)) || b == '_'
}

// hasValuesKeyword reports whether qs[i:] starts with the word VALUES,
// delimited by non-identifier bytes on both sides.
func hasValuesKeyword(qs []byte, i int) bool {
	const kw = "values"
	if i+len(kw) > len(qs) {
		return false
	}
	for j := 0; j < len(kw); j++ {
		c := qs[i+j]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != kw[j] {
			return false
		}
	}
	if i > 0 && isIdentRune(qs[i-1]) {
		return false
	}
	if i+len(kw) < len(qs) && isIdentRune(qs[i+len(kw)]) {
		return false
	}
	return true
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// parseNamedInsert parses a named INSERT ... VALUES (...) template into
// literal and bind parameter segments.  Unlike compileNamedQuery it is aware
// of string literals and comments, so neither named parameters nor the VALUES
// keyword are recognized inside them, and the VALUES tuple is located
// structurally (by matching parentheses) rather than by splitting on commas.
func parseNamedInsert(query string) (*namedInsertTemplate, error) {
	qs := []byte(query)
	var parts []namedTemplatePart
	var lit []byte
	inTuple := false
	seenTuple := false
	expectTuple := false
	depth := 0

	flush := func() {
		if len(lit) > 0 {
			parts = append(parts, namedTemplatePart{literal: string(lit), inTuple: inTuple})
			lit = nil
		}
	}

	i := 0
	for i < len(qs) {
		b := qs[i]
		switch {
		// string literals and quoted identifiers, with doubled-quote escapes
		case b == '\'' || b == '"' || b == '`':
			lit = append(lit, b)
			i++
			for i < len(qs) {
				lit = append(lit, qs[i])
				if qs[i] == b {
					if i+1 < len(qs) && qs[i+1] == b {
						lit = append(lit, qs[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		// line comments
		case b == '-' && i+1 < len(qs) && qs[i+1] == '-':
			for i < len(qs) && qs[i] != '\n' {
				lit = append(lit, qs[i])
				i++
			}
		// block comments
		case b == '/' && i+1 < len(qs) && qs[i+1] == '*':
			lit = append(lit, '/', '*')
			i += 2
			for i < len(qs) {
				if qs[i] == '*' && i+1 < len(qs) && qs[i+1] == '/' {
					lit = append(lit, '*', '/')
					i += 2
					break
				}
				lit = append(lit, qs[i])
				i++
			}
		// the VALUES keyword, only honored before the tuple has been seen
		case !seenTuple && !expectTuple && (b == 'v' || b == 'V') && hasValuesKeyword(qs, i):
			expectTuple = true
			lit = append(lit, qs[i:i+6]...)
			i += 6
		// named bind parameters, with the :: and := escapes of compileNamedQuery
		case b == ':':
			switch {
			case i+1 < len(qs) && qs[i+1] == ':':
				lit = append(lit, ':')
				i += 2
			case i+1 < len(qs) && qs[i+1] == '=':
				lit = append(lit, ':', '=')
				i += 2
			case i+1 < len(qs) && isBindRune(qs[i+1]):
				j := i + 2
				for j < len(qs) && isBindRune(qs[j]) {
					j++
				}
				flush()
				parts = append(parts, namedTemplatePart{name: string(qs[i+1 : j]), inTuple: inTuple})
				i = j
			default:
				lit = append(lit, ':')
				i++
			}
		case b == '(':
			if expectTuple {
				flush()
				inTuple = true
				seenTuple = true
				expectTuple = false
				depth = 1
				lit = append(lit, '(')
				i++
			} else {
				if inTuple {
					depth++
				}
				lit = append(lit, b)
				i++
			}
		case b == ')':
			lit = append(lit, b)
			i++
			if inTuple {
				depth--
				if depth == 0 {
					flush()
					inTuple = false
				}
			}
		default:
			if expectTuple && !isSpaceByte(b) {
				expectTuple = false
			}
			lit = append(lit, b)
			i++
		}
	}
	flush()

	start, end := -1, -1
	for idx, p := range parts {
		if p.inTuple {
			if start == -1 {
				start = idx
			}
			end = idx
		}
	}
	if start == -1 {
		return nil, errors.New("sqlx: NamedExecBatches requires an INSERT ... VALUES (...) template")
	}

	t := &namedInsertTemplate{
		prefixParts: parts[:start],
		tupleParts:  parts[start : end+1],
		suffixParts: parts[end+1:],
	}
	for _, p := range t.prefixParts {
		if p.name != "" {
			t.prefixNames = append(t.prefixNames, p.name)
		}
	}
	for _, p := range t.tupleParts {
		if p.name != "" {
			t.rowNames = append(t.rowNames, p.name)
		}
	}
	for _, p := range t.suffixParts {
		if p.name != "" {
			t.suffixNames = append(t.suffixNames, p.name)
		}
	}
	if len(t.rowNames) == 0 {
		return nil, errors.New("sqlx: NamedExecBatches requires at least one named parameter inside VALUES (...)")
	}
	return t, nil
}

// chunkQuery assembles the statement for a batch of rows rows, emitting bind
// parameters in the style of bindType.  Numbered bind styles (DOLLAR, AT) are
// numbered from 1 for every batch.
func (t *namedInsertTemplate) chunkQuery(bindType int, rows int) string {
	var buf bytes.Buffer
	currentVar := 1
	emit := func(p namedTemplatePart) {
		if p.name == "" {
			buf.WriteString(p.literal)
			return
		}
		switch bindType {
		case DOLLAR:
			buf.WriteByte('$')
			buf.WriteString(strconv.Itoa(currentVar))
			currentVar++
		case AT:
			buf.WriteString("@p")
			buf.WriteString(strconv.Itoa(currentVar))
			currentVar++
		case NAMED:
			buf.WriteByte(':')
			buf.WriteString(p.name)
		default:
			buf.WriteByte('?')
		}
	}
	for _, p := range t.prefixParts {
		emit(p)
	}
	for r := 0; r < rows; r++ {
		if r > 0 {
			buf.WriteByte(',')
		}
		for _, p := range t.tupleParts {
			emit(p)
		}
	}
	for _, p := range t.suffixParts {
		emit(p)
	}
	return buf.String()
}

// namedBatch is a single executable statement of a named INSERT batch.
type namedBatch struct {
	query string
	args  []interface{}
}

// bindNamedBatches compiles a named INSERT template and a slice of struct or
// map rows into a list of executable batches.  Every batch stays within the
// maxParams bind parameter budget, which accounts for the parameters repeated
// per row inside the VALUES tuple (including repeated names) as well as the
// fixed parameters appearing before or after it.  Bind parameters outside
// the VALUES tuple are bound from the first row of each batch.
//
// All rows are bound before this function returns, so a binding failure
// guarantees that no statement is executed by the caller.
func bindNamedBatches(bindType int, query string, arg interface{}, m *reflectx.Mapper, maxParams int) ([]namedBatch, error) {
	tmpl, err := parseNamedInsert(query)
	if err != nil {
		return nil, err
	}
	if maxParams < 1 {
		return nil, fmt.Errorf("sqlx: NamedExecBatches maxParams must be >= 1, got %d", maxParams)
	}
	fixed := len(tmpl.prefixNames) + len(tmpl.suffixNames)
	perRow := len(tmpl.rowNames)
	rowsPerBatch := (maxParams - fixed) / perRow
	if rowsPerBatch < 1 {
		return nil, fmt.Errorf("sqlx: NamedExecBatches maxParams %d is too small: one row needs %d parameters and the statement needs %d more", maxParams, perRow, fixed)
	}

	v := reflect.ValueOf(arg)
	for v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return nil, fmt.Errorf("sqlx: NamedExecBatches expected a slice or array of structs or maps, got %T", arg)
	}
	n := v.Len()
	if n == 0 {
		return nil, fmt.Errorf("sqlx: NamedExecBatches length of array is 0: %#v", arg)
	}

	// bind every row up front so that a binding failure sends no SQL at all
	rowArgs := make([][]interface{}, n)
	for i := 0; i < n; i++ {
		args, err := bindAnyArgs(tmpl.rowNames, v.Index(i).Interface(), m)
		if err != nil {
			return nil, fmt.Errorf("sqlx: NamedExecBatches binding row %d: %v", i, err)
		}
		rowArgs[i] = args
	}

	var batches []namedBatch
	for start := 0; start < n; start += rowsPerBatch {
		end := start + rowsPerBatch
		if end > n {
			end = n
		}
		first := v.Index(start).Interface()
		prefixArgs, err := bindAnyArgs(tmpl.prefixNames, first, m)
		if err != nil {
			return nil, fmt.Errorf("sqlx: NamedExecBatches binding row %d: %v", start, err)
		}
		suffixArgs, err := bindAnyArgs(tmpl.suffixNames, first, m)
		if err != nil {
			return nil, fmt.Errorf("sqlx: NamedExecBatches binding row %d: %v", start, err)
		}
		args := make([]interface{}, 0, fixed+perRow*(end-start))
		args = append(args, prefixArgs...)
		for i := start; i < end; i++ {
			args = append(args, rowArgs[i]...)
		}
		args = append(args, suffixArgs...)
		batches = append(batches, namedBatch{
			query: tmpl.chunkQuery(bindType, end-start),
			args:  args,
		})
	}
	return batches, nil
}
