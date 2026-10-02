package sqlx

// Binding-only named parameter inspection.
//
// This file adds a detailed, side effect free report for named query
// binding.  It shares the field resolution rules of named.go and reflectx
// (same mapper, same struct tags, same anonymous embedded struct shadowing)
// but it never touches a database, never allocates or mutates values inside
// the argument and never calls driver.Valuer.Value (or any other user code)
// on the bound values.
//
// Unlike compileNamedQuery, the placeholder scanner used here is SQL aware:
// colons inside string literals, quoted identifiers and comments are left
// untouched, and PostgreSQL style casts ("::type") are not mistaken for
// placeholders.

import (
	"errors"
	"reflect"
	"strconv"
	"unicode"

	"github.com/jmoiron/sqlx/reflectx"
)

// Placeholder describes a single named placeholder occurrence in a query,
// in the order it appears.  A name used more than once produces one entry
// per occurrence, exactly as it produces one bound argument per occurrence.
type Placeholder struct {
	// Order is the zero based position of this placeholder among all
	// placeholder occurrences in the query.
	Order int

	// Name is the placeholder name exactly as written after the colon
	// (without the colon itself).
	Name string

	// Source describes where the value came from: the map key for map
	// arguments or the dotted Go struct field path for struct arguments.
	// It is empty for missing parameters.
	Source string

	// Found reports whether a value was supplied for this occurrence.
	// An explicit nil, a zero value and an empty slice all count as found;
	// only a completely missing key or field is reported as not found.
	Found bool

	// Value is the bound value for this occurrence.  It is nil for missing
	// parameters; note that an explicit nil argument is also nil but is
	// reported with Found == true.  The value is read only: neither
	// driver.Valuer.Value nor any other user method is ever called on it.
	Value interface{}
}

// BindReport is a detailed binding-only description of a named query.  It
// contains everything that can be learned about the binding without
// executing the query or evaluating any user supplied conversion code.
type BindReport struct {
	// Query is the query with named placeholders rewritten for bindType.
	// Colons in string literals, quoted identifiers, comments and casts are
	// preserved verbatim.
	Query string

	// Placeholders lists every placeholder occurrence in query order.
	// Repeated names produce repeated entries (and repeated Args).
	Placeholders []Placeholder

	// Args is the positional argument list that would be passed alongside
	// Query.  Entries for missing parameters are nil.
	Args []interface{}

	// Missing lists the names that were used in the query but not supplied,
	// in the order of their first occurrence.  Nil values, zero values and
	// empty slices are never considered missing.
	Missing []string
}

// HasMissing reports whether any placeholder had no matching argument.
func (r *BindReport) HasMissing() bool { return len(r.Missing) > 0 }

// CountNamed counts named placeholders in query without binding values and
// without any side effects.  It returns one entry per placeholder
// occurrence, so a repeated name is counted once per use, while colons in
// string literals, quoted identifiers, comments and "::" casts are ignored.
func CountNamed(query string) ([]string, error) {
	_, names, err := scanNamedQuery([]byte(query), QUESTION)
	return names, err
}

// BindNamedReport produces a detailed binding-only report for query using
// arg, which must be a map with a string key or a struct.  Values are never
// converted: driver.Valuer is not invoked, the database is never accessed
// and nil pointers inside struct arguments are never allocated.
//
// Missing keys or fields are reported rather than returned as an error.
// An explicit nil value, a zero value and an empty slice are all reported
// as present; only entirely absent names are reported missing.
func BindNamedReport(bindType int, query string, arg interface{}) (*BindReport, error) {
	return bindNamedReportMapper(bindType, query, arg, mapper())
}

func bindNamedReportMapper(bindType int, query string, arg interface{}, m *reflectx.Mapper) (*BindReport, error) {
	if arg == nil {
		return nil, errors.New("sqlx.BindNamedReport: nil argument")
	}
	report := &BindReport{
		Placeholders: nil,
		Args:         nil,
	}

	// A single scan both rewrites the query and extracts placeholder names.
	var names []string
	var err error
	report.Query, names, err = scanNamedQuery([]byte(query), bindType)
	if err != nil {
		return nil, err
	}
	report.Placeholders = make([]Placeholder, 0, len(names))
	report.Args = make([]interface{}, 0, len(names))

	if maparg, ok := convertMapStringInterface(arg); ok {
		fillMapReport(report, names, maparg)
		return report, nil
	}

	if err := fillStructReport(report, names, arg, m); err != nil {
		return nil, err
	}
	return report, nil
}

func fillMapReport(report *BindReport, names []string, arg map[string]interface{}) {
	missingSeen := map[string]bool{}
	for i, name := range names {
		p := Placeholder{Order: i, Name: name}
		if val, ok := arg[name]; ok {
			p.Found = true
			p.Source = name
			p.Value = val
			report.Args = append(report.Args, val)
		} else {
			report.Args = append(report.Args, nil)
			if !missingSeen[name] {
				missingSeen[name] = true
				report.Missing = append(report.Missing, name)
			}
		}
		report.Placeholders = append(report.Placeholders, p)
	}
}

func fillStructReport(report *BindReport, names []string, arg interface{}, m *reflectx.Mapper) error {
	t := reflect.TypeOf(arg)
	if k := t.Kind(); k != reflect.Struct && (k != reflect.Ptr || reflectx.Deref(t).Kind() != reflect.Struct) {
		// mirrors bindNamedMapper: only maps and structs are supported here
		return errors.New("sqlx.BindNamedReport: unsupported argument type: " + t.String())
	}

	// Indirect pointers without allocating through them: a nil embedded
	// pointer keeps the field readable as a nil value instead of being
	// materialized.
	v := reflect.ValueOf(arg)
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			// A nil top level pointer behaves like a struct of zero values,
			// matching bindArgs' read only use of the argument.
			v = reflect.New(v.Type().Elem()).Elem()
			break
		}
		v = v.Elem()
	}

	structMap := m.TypeMap(v.Type())
	missingSeen := map[string]bool{}

	return m.TraversalsByNameFunc(v.Type(), names, func(i int, traversal []int) error {
		name := names[i]
		p := Placeholder{Order: i, Name: name}
		if len(traversal) == 0 {
			report.Args = append(report.Args, nil)
			if !missingSeen[name] {
				missingSeen[name] = true
				report.Missing = append(report.Missing, name)
			}
			report.Placeholders = append(report.Placeholders, p)
			return nil
		}

		val := fieldByIndexesSafe(v, traversal)
		if fi := structMap.GetByTraversal(traversal); fi != nil {
			p.Source = goFieldPath(v.Type(), fi)
		}
		p.Found = true
		if val.IsValid() {
			// Interface() never calls a user method; it exposes the stored
			// value as is, leaving driver.Valuer entirely untouched.
			p.Value = val.Interface()
		}
		report.Args = append(report.Args, p.Value)
		report.Placeholders = append(report.Placeholders, p)
		return nil
	})
}

// fieldByIndexesSafe follows a struct traversal read only, like
// reflectx.FieldByIndexesReadOnly, but a nil pointer in the middle yields an
// invalid Value instead of panicking; nothing is ever allocated.
func fieldByIndexesSafe(v reflect.Value, indexes []int) reflect.Value {
	for _, i := range indexes {
		if v.Kind() == reflect.Ptr {
			if v.IsNil() {
				return reflect.Value{}
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	return v
}

// goFieldPath renders the embedded-aware Go field path of a mapped field,
// e.g. "reportOuter.Inner.Name", so report consumers can see which struct
// field won an anonymous embedded shadowing conflict.
func goFieldPath(root reflect.Type, fi *reflectx.FieldInfo) string {
	var parts []string
	for p := fi; p != nil && p.Field.Name != ""; p = p.Parent {
		parts = append(parts, p.Field.Name)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	path := root.Name()
	for _, part := range parts {
		path += "." + part
	}
	return path
}

// scanNamedQuery is an SQL aware variant of compileNamedQuery.  It skips
// string literals, quoted identifiers and comments, so colons in them are
// never treated as placeholders, and it leaves "::" cast syntax intact.
func scanNamedQuery(qs []byte, bindType int) (query string, names []string, err error) {
	names = make([]string, 0, 10)
	rebound := make([]byte, 0, len(qs))

	currentVar := 1
	name := make([]byte, 0, 10)

	flush := func(terminator byte) {
		// emit the bindvar for the placeholder just finished
		switch bindType {
		case NAMED:
			rebound = append(rebound, ':')
			rebound = append(rebound, name...)
		case QUESTION, UNKNOWN:
			rebound = append(rebound, '?')
		case DOLLAR:
			rebound = append(rebound, '$')
			rebound = append(rebound, strconv.Itoa(currentVar)...)
			currentVar++
		case AT:
			rebound = append(rebound, '@', 'p')
			rebound = append(rebound, strconv.Itoa(currentVar)...)
			currentVar++
		}
		names = append(names, string(name))
		name = name[:0]
		// the terminator is ordinary query text (or nothing at EOF)
		if terminator != 0 {
			rebound = append(rebound, terminator)
		}
	}

	for i := 0; i < len(qs); {
		b := qs[i]

		// single quoted string literal; '' is an escaped quote and
		// backslash escapes the next byte
		if b == '\'' {
			rebound = append(rebound, b)
			i++
			for i < len(qs) {
				c := qs[i]
				rebound = append(rebound, c)
				i++
				if c == '\\' && i < len(qs) {
					rebound = append(rebound, qs[i])
					i++
					continue
				}
				if c == '\'' {
					if i < len(qs) && qs[i] == '\'' {
						rebound = append(rebound, qs[i])
						i++
						continue
					}
					break
				}
			}
			continue
		}

		// double quoted identifier; "" is an escaped quote
		if b == '"' {
			rebound = append(rebound, b)
			i++
			for i < len(qs) {
				c := qs[i]
				rebound = append(rebound, c)
				i++
				if c == '"' {
					if i < len(qs) && qs[i] == '"' {
						rebound = append(rebound, qs[i])
						i++
						continue
					}
					break
				}
			}
			continue
		}

		// backtick quoted identifier; `` is an escaped quote
		if b == '`' {
			rebound = append(rebound, b)
			i++
			for i < len(qs) {
				c := qs[i]
				rebound = append(rebound, c)
				i++
				if c == '`' {
					if i < len(qs) && qs[i] == '`' {
						rebound = append(rebound, qs[i])
						i++
						continue
					}
					break
				}
			}
			continue
		}

		// line comment: "-- ..." or MySQL's "# ..."
		if (b == '-' && i+1 < len(qs) && qs[i+1] == '-') || b == '#' {
			for i < len(qs) && qs[i] != '\n' {
				rebound = append(rebound, qs[i])
				i++
			}
			continue
		}

		// block comment: /* ... */
		if b == '/' && i+1 < len(qs) && qs[i+1] == '*' {
			for i < len(qs) {
				if qs[i] == '*' && i+1 < len(qs) && qs[i+1] == '/' {
					rebound = append(rebound, '*', '/')
					i += 2
					break
				}
				rebound = append(rebound, qs[i])
				i++
			}
			continue
		}

		if b == ':' {
			// PostgreSQL cast: "::" is never a placeholder
			if i+1 < len(qs) && qs[i+1] == ':' {
				rebound = append(rebound, ':', ':')
				i += 2
				continue
			}
			// MySQL style assignment ":=" stays verbatim
			if i+1 < len(qs) && qs[i+1] == '=' {
				rebound = append(rebound, ':', '=')
				i += 2
				continue
			}

			// read the placeholder name; names share the allowedBindRunes
			// rules used by compileNamedQuery (letters, digits, underscore,
			// dotted traversal paths)
			i++
			for i < len(qs) {
				r := rune(qs[i])
				if qs[i] < 0x80 {
					if !(unicode.IsOneOf(allowedBindRunes, r) || qs[i] == '_' || qs[i] == '.') {
						break
					}
				} else if !unicode.IsOneOf(allowedBindRunes, r) {
					// be conservative on multibyte runes: accept letters and
					// digits rather than splitting a UTF-8 sequence
					break
				}
				name = append(name, qs[i])
				i++
			}
			terminator := byte(0)
			if i < len(qs) {
				terminator = qs[i]
			}
			// a cast directly following a placeholder terminates it but the
			// "::" itself must remain raw text
			if terminator == ':' && i+1 < len(qs) && qs[i+1] == ':' {
				flush(0)
				rebound = append(rebound, ':', ':')
				i += 2
				continue
			}
			if len(name) == 0 {
				// lone ":" with no name (e.g. ": "); treat as plain text
				rebound = append(rebound, ':')
				if terminator != 0 {
					rebound = append(rebound, terminator)
				}
				if i < len(qs) {
					i++
				}
				continue
			}
			flush(terminator)
			if terminator != 0 {
				i++
			}
			continue
		}

		rebound = append(rebound, b)
		i++
	}

	return string(rebound), names, err
}
