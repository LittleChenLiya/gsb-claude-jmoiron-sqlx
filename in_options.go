package sqlx

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// EmptySlicePolicy controls how InWithOptions treats slice arguments that
// contain no elements.
type EmptySlicePolicy int

const (
	// EmptySliceError makes InWithOptions reject empty slice arguments with
	// an error wrapping ErrEmptySlice.  This mirrors the behavior of In.
	EmptySliceError EmptySlicePolicy = iota
	// EmptySliceNull makes InWithOptions expand an empty slice argument to
	// the single SQL literal NULL instead of a bindvar, so that
	// "id in (?)" becomes "id in (NULL)" and the slice contributes no
	// arguments to the final argument list.
	EmptySliceNull
)

// ErrEmptySlice is returned (wrapped) by InWithOptions when an empty slice
// argument is encountered and the EmptySlice policy is EmptySliceError.
// Use errors.Is to test for it.
var ErrEmptySlice = errors.New("sqlx: empty slice passed to 'in' query")

// InOptions configures the behavior of InWithOptions.
type InOptions struct {
	// EmptySlice selects how empty slice arguments are handled.  The zero
	// value is EmptySliceError, which rejects them like In does.
	EmptySlice EmptySlicePolicy
}

// inArgMeta stores, for a single argument, its reflect.Value and length when
// it is an expandable slice, or the plain value otherwise.
type inArgMeta struct {
	v       reflect.Value
	i       interface{}
	length  int
	isSlice bool
}

// findBindVars returns the byte offsets of every '?' bindvar in query,
// skipping '?' characters that appear inside string literals quoted with
// single quotes, double quotes or backticks.  Backslash escapes and doubled
// quote characters inside a literal are honored.
func findBindVars(query string) []int {
	var positions []int
	for i := 0; i < len(query); i++ {
		switch c := query[i]; c {
		case '\'', '"', '`':
			i++
			for i < len(query) {
				if query[i] == '\\' && c != '`' {
					i += 2
					continue
				}
				if query[i] == c {
					// a doubled quote character is an escaped quote
					if i+1 < len(query) && query[i+1] == c {
						i += 2
						continue
					}
					break
				}
				i++
			}
		case '?':
			positions = append(positions, i)
		}
	}
	return positions
}

// InWithOptions expands slice values in args like In, but lets the caller
// explicitly configure how empty slices are handled via opts.
//
// Unlike In, InWithOptions validates the whole query before producing any
// output: every driver.Valuer argument is resolved exactly once, every empty
// slice is checked against opts.EmptySlice, and the number of '?' bindvars
// (ignoring '?' characters inside quoted string literals) is checked against
// the number of arguments.  If any check fails, InWithOptions returns an
// error and no partial query; the args slice passed by the caller is never
// modified.
//
// Only real slice arguments are expanded: []byte stays a single value, as
// does any driver.Valuer resolving to one.  With EmptySliceNull an empty
// slice expands to the SQL literal NULL and contributes no bindvar.
func InWithOptions(query string, opts InOptions, args ...interface{}) (string, []interface{}, error) {
	meta := make([]inArgMeta, len(args))

	var flatArgsCount int
	var anySlices bool

	// Pass 1: resolve driver.Valuer arguments exactly once and classify
	// each argument without mutating the caller's args slice.
	for i, arg := range args {
		if a, ok := arg.(driver.Valuer); ok {
			v, err := a.Value()
			if err != nil {
				return "", nil, err
			}
			arg = v
		}

		if v, ok := asSliceForIn(arg); ok {
			meta[i].isSlice = true
			meta[i].v = v
			meta[i].length = v.Len()
			anySlices = true
			flatArgsCount += meta[i].length
		} else {
			meta[i].i = arg
			flatArgsCount++
		}
	}

	// Pass 2: enforce the empty slice policy for every slice argument
	// before any SQL is generated.
	for i := range meta {
		if meta[i].isSlice && meta[i].length == 0 {
			switch opts.EmptySlice {
			case EmptySliceError:
				return "", nil, fmt.Errorf("%w: argument %d", ErrEmptySlice, i)
			case EmptySliceNull:
				// expanded to NULL below, contributes no bindvar
			default:
				return "", nil, fmt.Errorf("sqlx: unknown empty slice policy %d", int(opts.EmptySlice))
			}
		}
	}

	// Pass 3: count bindvars, ignoring '?' inside quoted string literals,
	// and verify the argument count before generating anything.
	bindvars := findBindVars(query)
	if len(bindvars) > len(meta) {
		return "", nil, fmt.Errorf("sqlx: number of bindVars (%d) exceeds arguments (%d)", len(bindvars), len(meta))
	}
	if len(bindvars) < len(meta) {
		return "", nil, fmt.Errorf("sqlx: number of bindVars (%d) less than number of arguments (%d)", len(bindvars), len(meta))
	}

	// Nothing to expand; return the caller's values untouched.
	if !anySlices {
		return query, args, nil
	}

	// Pass 4: generate the new query and argument list.
	newArgs := make([]interface{}, 0, flatArgsCount)

	var buf strings.Builder
	buf.Grow(len(query) + len(", ?")*flatArgsCount)

	prev := 0
	for i, pos := range bindvars {
		m := meta[i]

		buf.WriteString(query[prev:pos])

		switch {
		case !m.isSlice:
			buf.WriteByte('?')
			newArgs = append(newArgs, m.i)
		case m.length == 0:
			// EmptySliceNull was validated above.
			buf.WriteString("NULL")
		default:
			buf.WriteByte('?')
			for si := 1; si < m.length; si++ {
				buf.WriteString(", ?")
			}
			newArgs = appendReflectSlice(newArgs, m.v, m.length)
		}

		prev = pos + 1
	}

	buf.WriteString(query[prev:])

	return buf.String(), newArgs, nil
}
