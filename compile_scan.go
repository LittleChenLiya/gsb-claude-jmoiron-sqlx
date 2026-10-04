package sqlx

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jmoiron/sqlx/reflectx"
)

// ColumnField explicitly maps a single result column, identified by its
// ordinal position (zero based) in the result set, to a destination struct
// field. Field names follow the Mapper's name rules: a top level field is
// referenced by its struct name (e.g. "UserID") and a field inside an
// embedded struct by its dot separated path (e.g. "Team.ID"). The Mapper is
// still used for the lookup, so anonymous embedded fields keep their original
// shallowest-wins priority.
type ColumnField struct {
	// Column is the zero based ordinal position of the result column.
	Column int
	// Field is the destination struct field name or dot separated path.
	Field string
}

// CompileStructScanOption customizes a plan produced by CompileStructScan.
type CompileStructScanOption struct {
	apply func(*compileConfig)
}

type compileConfig struct {
	mapper  *reflectx.Mapper
	mapping []ColumnField
}

// WithCompileMapper uses the provided Mapper instead of the package default
// Mapper (or the Mapper carried by an sqlx.DB / sqlx.Tx).
func WithCompileMapper(m *reflectx.Mapper) CompileStructScanOption {
	return CompileStructScanOption{apply: func(c *compileConfig) {
		if m != nil {
			c.mapper = m
		}
	}}
}

// WithColumnFields explicitly assigns result column ordinals to destination
// fields. Columns not listed here are still resolved from their column name
// through the Mapper. The same destination field may never be assigned more
// than once, neither by two explicit entries nor by an explicit entry and an
// implicitly mapped column.
func WithColumnFields(mapping ...ColumnField) CompileStructScanOption {
	return CompileStructScanOption{apply: func(c *compileConfig) {
		c.mapping = append(c.mapping, mapping...)
	}}
}

// ColumnMapping describes the resolved destination of one result column.
type ColumnMapping struct {
	// Column is the zero based ordinal position of the result column.
	Column int
	// Name is the result column name.
	Name string
	// Field is the destination struct field name or dot separated path.
	Field string
	// Traversal is the index path used to reach the field via reflect.
	Traversal []int
}

// StructScanPlan is a compiled, validated mapping of result columns to struct
// fields. It is produced by CompileStructScan without reading any rows and can
// be reused for many rows and many destination values of the same type. A
// zero value StructScanPlan must not be used.
type StructScanPlan struct {
	destType   reflect.Type
	columns    []string
	traversals [][]int
	mappings   []ColumnMapping
}

// CompileStructScan compiles a mapping of result columns to the fields of the
// struct pointed to by dest and validates it before any row is scanned.
//
// Compilation is strict by default, which fixes a shortcoming of the regular
// StructScan behavior: when a join returns several columns with the same name
// (for example [id, id, name]), StructScan silently points every one of them
// at the same field and the columns overwrite each other. CompileStructScan
// instead reports every column position involved in such a conflict, together
// with the target field. Conflicts can be resolved explicitly with
// WithColumnFields, which assigns each column ordinal to a distinct field.
//
// Rules enforced during compilation:
//   - every column must have a destination field (columns without one are
//     reported by position and name),
//   - two columns may never resolve to the same destination field; the error
//     reports all column positions involved and the target field,
//   - explicit ColumnField ordinals must be in range and their fields must
//     exist.
//
// No rows are read during compilation, and the default behavior of
// StructScan/Get/Select is unchanged.
func CompileStructScan(dest interface{}, columns []string, opts ...CompileStructScanOption) (*StructScanPlan, error) {
	cfg := &compileConfig{mapper: mapper()}
	for _, opt := range opts {
		opt.apply(cfg)
	}
	m := cfg.mapper
	if m == nil {
		m = mapper()
	}

	t := reflect.TypeOf(dest)
	if t == nil {
		return nil, errors.New("nil interface passed to CompileStructScan")
	}
	if t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("must pass a pointer to a struct to CompileStructScan, got %T", dest)
	}
	t = t.Elem()

	tm := m.TypeMap(t)
	// traversals[i] is the chosen destination for column i.
	traversals := make([][]int, len(columns))
	fieldNames := make([]string, len(columns))

	// explicit, ordinal based assignments take precedence
	for _, cf := range cfg.mapping {
		if cf.Column < 0 || cf.Column >= len(columns) {
			return nil, fmt.Errorf("column ordinal %d out of range for result with %d column(s)", cf.Column, len(columns))
		}
		fi := tm.GetByPath(cf.Field)
		if fi == nil || fi.Embedded {
			return nil, fmt.Errorf("unknown destination field %q (column %d) in %s", cf.Field, cf.Column, t)
		}
		traversals[cf.Column] = append([]int(nil), fi.Index...)
		fieldNames[cf.Column] = cf.Field
	}

	// resolve the remaining columns from their names with the Mapper, so that
	// anonymous embedded field priority is exactly the original one
	for i, name := range columns {
		if traversals[i] != nil {
			continue
		}
		fi, ok := tm.Names[name]
		if !ok {
			return nil, fmt.Errorf("missing destination name %q (column %d) in %s", name, i, t)
		}
		traversals[i] = append([]int(nil), fi.Index...)
		fieldNames[i] = fi.Path
	}

	// every destination field may only be written by a single column
	owners := map[string]int{}
	for i, tr := range traversals {
		key := traversalKey(tr)
		if prev, ok := owners[key]; ok {
			return nil, fmt.Errorf(
				"columns %d (%q) and %d (%q) both map to destination field %q in %s; use WithColumnFields to assign each column ordinal explicitly",
				prev, columns[prev], i, columns[i], fieldNames[i], t)
		}
		owners[key] = i
	}

	mappings := make([]ColumnMapping, len(columns))
	for i := range columns {
		mappings[i] = ColumnMapping{
			Column:    i,
			Name:      columns[i],
			Field:     fieldNames[i],
			Traversal: traversals[i],
		}
	}

	return &StructScanPlan{
		destType:   t,
		columns:    append([]string(nil), columns...),
		traversals: traversals,
		mappings:   mappings,
	}, nil
}

// Columns returns the compiled column to field mappings in column order. It
// can be used to print or otherwise inspect the compiled mapping before any
// row is scanned.
func (p *StructScanPlan) Columns() []ColumnMapping {
	out := make([]ColumnMapping, len(p.mappings))
	copy(out, p.mappings)
	return out
}

// Scan scans the current row of r into the struct pointed to by dest using
// the compiled mapping. r is typically an *sqlx.Rows (between Next calls) or
// an *sqlx.Row.
//
// The destination is updated atomically: the row is first scanned into a
// throwaway copy of dest, and it replaces the original only after every
// column converted successfully. If a column fails to convert, dest is left
// completely untouched (earlier columns included) and no partially converted
// struct is visible. Fields not covered by the plan are preserved.
func (p *StructScanPlan) Scan(r ColScanner, dest interface{}) error {
	if p == nil {
		return errors.New("Scan called on a nil StructScanPlan")
	}
	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Ptr {
		return errors.New("must pass a pointer, not a value, to StructScanPlan.Scan destination")
	}
	if v.IsNil() {
		return errors.New("nil pointer passed to StructScanPlan.Scan destination")
	}
	if e := v.Type().Elem(); e != p.destType {
		return fmt.Errorf("StructScanPlan compiled for %s but got %s", p.destType, e)
	}

	// guard against a result set whose shape differs from the compiled one
	cols, err := r.Columns()
	if err != nil {
		return err
	}
	if len(cols) != len(p.columns) {
		return fmt.Errorf("StructScanPlan compiled for %d column(s) but result has %d", len(p.columns), len(cols))
	}

	// scan into a throwaway copy so a conversion failure never leaves the
	// original destination partially updated
	orig := v.Elem()
	tmp := reflect.New(p.destType).Elem()
	tmp.Set(orig)
	// a shallow copy still shares every pointer-embedded struct with the
	// original; clone them in the copy so writes cannot leak into the original
	clonePointerParents(tmp, p.traversals)

	values := make([]interface{}, len(p.columns))
	if err := fieldsByTraversal(tmp, p.traversals, values, true); err != nil {
		return err
	}
	if err := r.Scan(values...); err != nil {
		// conversion failed: the original destination keeps its old values
		return err
	}
	if err := r.Err(); err != nil {
		return err
	}

	orig.Set(tmp)
	return nil
}

// CompileStructScanFrom compiles a plan for the struct pointed to by dest
// using the columns reported by r. Columns are the only thing read from r;
// no rows are fetched. It is a convenience for scanners such as *sqlx.Rows
// where the result columns are already known.
func CompileStructScanFrom(r ColScanner, dest interface{}, opts ...CompileStructScanOption) (*StructScanPlan, error) {
	columns, err := r.Columns()
	if err != nil {
		return nil, err
	}
	return CompileStructScan(dest, columns, opts...)
}

func traversalKey(traversal []int) string {
	var b strings.Builder
	for i, x := range traversal {
		if i > 0 {
			b.WriteByte('.')
		}
		fmt.Fprintf(&b, "%d", x)
	}
	return b.String()
}

// clonePointerParents replaces every non-nil pointer struct field traversed by
// the plan with a fresh copy of its element, so the throwaway destination does
// not share embedded structs with the original.
func clonePointerParents(v reflect.Value, traversals [][]int) {
	for _, tr := range traversals {
		cur := v
		for _, i := range tr[:len(tr)-1] {
			f := cur.Field(i)
			if f.Kind() == reflect.Ptr && !f.IsNil() {
				clone := reflect.New(f.Type().Elem())
				clone.Elem().Set(f.Elem())
				f.Set(clone)
				cur = clone.Elem()
				continue
			}
			cur = f
		}
	}
}
