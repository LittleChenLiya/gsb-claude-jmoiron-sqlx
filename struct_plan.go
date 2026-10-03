package sqlx

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jmoiron/sqlx/reflectx"
)

// ColumnMapping is a compiled plan that maps the columns of a result set to
// the fields of a struct type.  It is created by CompileColumnMapping and can
// be used to scan rows repeatedly without re-resolving column names.
//
// Unlike StructScan, a ColumnMapping is compiled before any row data is read,
// so ambiguous or duplicate column targets are reported as errors instead of
// silently overwriting struct fields.  Scanning is performed into temporary
// values and only committed to the destination struct after the driver has
// converted every column successfully, which means a conversion failure never
// leaves a partially updated struct behind.
type ColumnMapping struct {
	t       reflect.Type
	columns []string
	// fields holds the target FieldInfo for each result column.  A nil entry
	// means the column is discarded (only possible with WithUnsafe).
	fields []*reflectx.FieldInfo
}

// MappingOption configures CompileColumnMapping.
type MappingOption func(*mappingConfig)

type mappingConfig struct {
	explicit map[int]string
	unsafe   bool
}

// WithExplicitColumns supplies an explicit mapping from result column index
// (0-based) to struct field name.  Explicit mappings bypass name based lookup
// for the given columns, but they do not relax the duplicate target check: two
// columns may never resolve to the same struct field.
func WithExplicitColumns(m map[int]string) MappingOption {
	return func(c *mappingConfig) {
		if c.explicit == nil {
			c.explicit = make(map[int]string, len(m))
		}
		for i, name := range m {
			c.explicit[i] = name
		}
	}
}

// WithExplicitColumn is the single column variant of WithExplicitColumns.
func WithExplicitColumn(index int, field string) MappingOption {
	return func(c *mappingConfig) {
		if c.explicit == nil {
			c.explicit = map[int]string{}
		}
		c.explicit[index] = field
	}
}

// WithUnsafe allows result columns that do not map to any struct field to be
// discarded instead of producing a missing destination error.  Even in unsafe
// mode a column may not be mapped to more than one target field.
func WithUnsafe() MappingOption {
	return func(c *mappingConfig) { c.unsafe = true }
}

// CompileColumnMapping compiles a mapping from result columns to fields of the
// struct type t using the given mapper.  The compile step only inspects the
// column names and the struct type; it does not read any row data.
//
// In the default (strict) mode every column must resolve to a distinct struct
// field.  Two columns that resolve to the same field (for example a join that
// produces two columns both named "id") are reported as an error that names
// the column indices and the conflicting target field.  Use WithExplicitColumns
// to map individual column indices to fields explicitly; the duplicate target
// check still applies.  Use WithUnsafe to discard columns that have no
// matching field instead of erroring.
func CompileColumnMapping(m *reflectx.Mapper, t reflect.Type, columns []string, opts ...MappingOption) (*ColumnMapping, error) {
	if m == nil {
		m = mapper()
	}
	t = reflectx.Deref(t)
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("sqlx: cannot compile column mapping: %s is not a struct type", t)
	}
	if len(columns) == 0 {
		return nil, errors.New("sqlx: cannot compile column mapping: no columns")
	}

	cfg := mappingConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	tm := m.TypeMap(t)
	fields := make([]*reflectx.FieldInfo, len(columns))
	// seen maps the target field path to the column index that first claimed
	// it, so that duplicate targets can be reported with both positions.
	seen := make(map[string]int, len(columns))

	for i, col := range columns {
		var fi *reflectx.FieldInfo
		if name, ok := cfg.explicit[i]; ok {
			fi = tm.Names[name]
			if fi == nil {
				return nil, fmt.Errorf("sqlx: column %d (%q): no field named %q in %s", i, col, name, t)
			}
		} else {
			fi = tm.Names[col]
			if fi == nil {
				if !cfg.unsafe {
					return nil, fmt.Errorf("sqlx: column %d (%q): missing destination name %q in %s", i, col, col, t)
				}
				// no field for this column; it will be discarded
				continue
			}
		}

		if prev, dup := seen[fi.Path]; dup {
			return nil, fmt.Errorf("sqlx: duplicate column mapping: columns %d (%q) and %d (%q) both map to struct field %q",
				prev, columns[prev], i, col, fi.Path)
		}
		seen[fi.Path] = i
		fields[i] = fi
	}

	return &ColumnMapping{
		t:       t,
		columns: append([]string(nil), columns...),
		fields:  fields,
	}, nil
}

// Columns returns a copy of the result column names the mapping was compiled
// for.
func (p *ColumnMapping) Columns() []string {
	return append([]string(nil), p.columns...)
}

// Fields returns the target field path for each column.  A nil entry means the
// column is discarded.
func (p *ColumnMapping) Fields() []string {
	out := make([]string, len(p.fields))
	for i, fi := range p.fields {
		if fi != nil {
			out[i] = fi.Path
		}
	}
	return out
}

// String renders a human readable column-to-field mapping.
func (p *ColumnMapping) String() string {
	var b strings.Builder
	for i, col := range p.columns {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "column %d: %q", i, col)
		if fi := p.fields[i]; fi != nil {
			fmt.Fprintf(&b, " -> %s", fi.Path)
		} else {
			b.WriteString(" -> (discarded)")
		}
	}
	return b.String()
}

// Scan scans the current row of r into dest using the compiled mapping.  The
// caller must advance r with Next() before calling Scan, just like
// sql.Rows.Scan.
//
// Scan never leaves a partially updated struct: every column is first scanned
// into a temporary value, and the temporary values are only written to the
// destination struct after the driver has converted all columns successfully.
// If the driver reports a conversion error, dest keeps its previous values.
func (p *ColumnMapping) Scan(r ColScanner, dest interface{}) error {
	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Ptr {
		return errors.New("sqlx: must pass a pointer, not a value, to ColumnMapping.Scan destination")
	}
	if v.IsNil() {
		return errors.New("sqlx: nil pointer passed to ColumnMapping.Scan destination")
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return errors.New("sqlx: ColumnMapping.Scan destination must be a pointer to a struct")
	}
	if v.Type() != p.t {
		return fmt.Errorf("sqlx: column mapping was compiled for %s, not %s", p.t, v.Type())
	}

	// make sure the result set still matches the compiled column layout
	if cols, err := r.Columns(); err != nil {
		return err
	} else if !reflect.DeepEqual(cols, p.columns) {
		return fmt.Errorf("sqlx: column mapping was compiled for columns %q but result has %q", p.columns, cols)
	}

	// Scan into temporary values so that a conversion failure cannot leave the
	// destination struct partially updated.
	values := make([]interface{}, len(p.fields))
	targets := make([]reflect.Value, len(p.fields))
	for i, fi := range p.fields {
		if fi == nil {
			values[i] = new(interface{})
			continue
		}
		f := reflectx.FieldByIndexes(v, fi.Index)
		targets[i] = f
		values[i] = reflect.New(f.Type()).Interface()
	}

	if err := r.Scan(values...); err != nil {
		return err
	}
	if err := r.Err(); err != nil {
		return err
	}

	// commit the temporary values to the destination struct
	for i, f := range targets {
		if f.IsValid() {
			f.Set(reflect.ValueOf(values[i]).Elem())
		}
	}
	return nil
}

// compile time assertion that *sql.Rows satisfies ColScanner
var _ ColScanner = (*sql.Rows)(nil)
