package sqlx

// Named Bind Reports
//
//  * ReportNamed - inspect a named query against a map or struct argument
//    without executing it
//  * CountNamed - count named parameter occurrences in a query

import (
	"fmt"
	"reflect"
	"strings"
	"unicode"

	"github.com/jmoiron/sqlx/reflectx"
)

// NamedParam Source values.
const (
	// NamedParamSourceMap marks a parameter whose value came from a map.
	NamedParamSourceMap = "map"
	// NamedParamSourceStruct marks a parameter whose value came from a struct.
	NamedParamSourceStruct = "struct"
)

// NamedParam is a single named parameter occurrence in a query.
type NamedParam struct {
	// Name is the parameter name, without the leading colon.
	Name string
	// Index is the 0-based position of this occurrence among all of the
	// query's named parameters, in order of appearance.
	Index int
	// Found reports whether a value for Name was present in the argument.
	// A nil value, a zero value or an empty slice is still found; only a
	// missing map key or an unresolvable struct field is not.
	Found bool
	// Source describes where the value was found: NamedParamSourceMap or
	// NamedParamSourceStruct. It is empty when Found is false.
	Source string
	// Value is the value the parameter would be bound to. It is nil when
	// Found is false. Reading it never executes user code; in particular
	// driver.Valuer.Value is never called.
	Value interface{}
}

// NamedBindReport is the result of inspecting a named query against an
// argument without executing it.
type NamedBindReport struct {
	// Query is the original query.
	Query string
	// Bound is the query with named parameters replaced by ? bindvars,
	// ready for Rebind. Colons inside string literals, quoted identifiers,
	// comments and :: type casts are left untouched.
	Bound string
	// Params are the named parameter occurrences in order of appearance;
	// a placeholder used more than once produces one entry per occurrence.
	Params []NamedParam
	// Found is the number of parameter occurrences that were resolved.
	Found int
	// Missing are the parameter names that could not be resolved, in
	// order of first appearance.
	Missing []string
}

// HasMissing reports whether any named parameter could not be resolved.
func (r *NamedBindReport) HasMissing() bool {
	return len(r.Missing) > 0
}

// CountNamed returns the number of named parameter occurrences in query.
// Colons inside string literals, quoted identifiers, comments and :: type
// casts are not counted. It performs no binding and has no side effects.
func CountNamed(query string) int {
	_, names := scanNamedQuery(query)
	return len(names)
}

// ReportNamed inspects the named parameters in query against arg, which
// must be a map[string]interface{} (or a named type convertible to one)
// or a struct (or a pointer to one). It reports, in placeholder order,
// whether each parameter was found and where its value came from, and
// lists the names that are missing.
//
// ReportNamed only performs binding: it never accesses the database and
// never calls driver.Valuer.Value, so user code is not executed. A nil
// value, a zero value or an empty slice is not reported as missing.
func ReportNamed(query string, arg interface{}) (*NamedBindReport, error) {
	bound, names := scanNamedQuery(query)

	report := &NamedBindReport{
		Query:  query,
		Bound:  bound,
		Params: make([]NamedParam, 0, len(names)),
	}

	missing := make(map[string]bool)
	addMissing := func(name string) {
		if !missing[name] {
			missing[name] = true
			report.Missing = append(report.Missing, name)
		}
	}

	if arg == nil {
		for i, name := range names {
			report.Params = append(report.Params, NamedParam{Name: name, Index: i})
			addMissing(name)
		}
		return report, nil
	}

	if maparg, ok := convertMapStringInterface(arg); ok {
		for i, name := range names {
			val, found := maparg[name]
			p := NamedParam{Name: name, Index: i, Value: val}
			if found {
				p.Found = true
				p.Source = NamedParamSourceMap
				report.Found++
			} else {
				addMissing(name)
			}
			report.Params = append(report.Params, p)
		}
		return report, nil
	}

	// Struct argument: reuse the reflectx traversal rules from bindArgs.
	v := reflect.ValueOf(arg)
	for v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("sqlx.ReportNamed: unsupported arg type %T (expected a map or a struct)", arg)
	}

	m := mapper()
	if err := m.TraversalsByNameFunc(v.Type(), names, func(i int, traversal []int) error {
		p := NamedParam{Name: names[i], Index: i}
		if len(traversal) == 0 {
			addMissing(names[i])
		} else {
			val := reflectx.FieldByIndexesReadOnly(v, traversal)
			if val.IsValid() {
				p.Value = val.Interface()
			}
			p.Found = true
			p.Source = NamedParamSourceStruct
			report.Found++
		}
		report.Params = append(report.Params, p)
		return nil
	}); err != nil {
		return nil, err
	}

	return report, nil
}

// scanNamedQuery scans q for named parameters while ignoring colons that
// appear inside SQL string literals, quoted identifiers, comments, or the
// :: type-cast operator. It returns the query with named parameters
// replaced by ? bindvars and the parameter names in order of appearance.
// A placeholder used more than once produces one name per occurrence.
func scanNamedQuery(q string) (string, []string) {
	names := make([]string, 0, 4)
	var b strings.Builder
	b.Grow(len(q))

	i := 0
	n := len(q)
	for i < n {
		c := q[i]
		switch {
		case c == '\'':
			// single-quoted string literal; doubled and backslash-escaped
			// quotes are kept verbatim
			b.WriteByte(c)
			i++
			for i < n {
				if q[i] == 92 && i+1 < n {
					b.WriteByte(q[i])
					b.WriteByte(q[i+1])
					i += 2
					continue
				}
				b.WriteByte(q[i])
				if q[i] == '\'' {
					i++
					if i < n && q[i] == '\'' {
						b.WriteByte(q[i])
						i++
						continue
					}
					break
				}
				i++
			}
		case c == '"':
			// double-quoted identifier; a doubled quote is kept verbatim
			b.WriteByte(c)
			i++
			for i < n {
				b.WriteByte(q[i])
				if q[i] == '"' {
					i++
					if i < n && q[i] == '"' {
						b.WriteByte(q[i])
						i++
						continue
					}
					break
				}
				i++
			}
		case c == '`':
			// backtick-quoted identifier; a doubled backtick is kept verbatim
			b.WriteByte(c)
			i++
			for i < n {
				b.WriteByte(q[i])
				if q[i] == '`' {
					i++
					if i < n && q[i] == '`' {
						b.WriteByte(q[i])
						i++
						continue
					}
					break
				}
				i++
			}
		case c == '-' && i+1 < n && q[i+1] == '-':
			// line comment: runs to the end of the line
			for i < n && q[i] != '\n' {
				b.WriteByte(q[i])
				i++
			}
		case c == '/' && i+1 < n && q[i+1] == '*':
			// block comment; nesting is supported
			b.WriteByte('/')
			b.WriteByte('*')
			i += 2
			depth := 1
			for i < n && depth > 0 {
				if q[i] == '/' && i+1 < n && q[i+1] == '*' {
					b.WriteByte('/')
					b.WriteByte('*')
					i += 2
					depth++
					continue
				}
				if q[i] == '*' && i+1 < n && q[i+1] == '/' {
					b.WriteByte('*')
					b.WriteByte('/')
					i += 2
					depth--
					continue
				}
				b.WriteByte(q[i])
				i++
			}
		case c == ':':
			if i+1 < n && q[i+1] == ':' {
				// type cast / escaped colon: keep both colons verbatim
				b.WriteByte(':')
				b.WriteByte(':')
				i += 2
				continue
			}
			if i+1 < n && q[i+1] == '=' {
				// assignment operator: keep verbatim
				b.WriteByte(':')
				b.WriteByte('=')
				i += 2
				continue
			}
			nameStart := i + 1
			j := nameStart
			for j < n && isNamedRune(q[j]) {
				j++
			}
			if j == nameStart {
				// a lone colon is not a parameter; keep it verbatim
				b.WriteByte(':')
				i++
				continue
			}
			names = append(names, q[nameStart:j])
			b.WriteByte('?')
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), names
}

// isNamedRune reports whether r can be part of a named parameter name:
// letters, digits, underscores and dots, matching the rules used by
// compileNamedQuery.
func isNamedRune(r byte) bool {
	return r == '_' || r == '.' || unicode.IsOneOf(allowedBindRunes, rune(r))
}
