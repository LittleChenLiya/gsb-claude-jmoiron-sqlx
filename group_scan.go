package sqlx

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx/reflectx"
)

// GroupConfig configures ScanGroups, which assembles the repeated rows of a
// JOIN result into parent structs, each holding a list of child structs.
//
// Result columns are partitioned by prefix. A column named "prefix.name"
// belongs to the corresponding struct and is mapped to the field found for
// "name". ParentPrefix and ChildPrefix must both be set and must differ.
// Columns matching neither prefix are rejected.
//
// ParentKeys and ChildKeys are full result column names, including the
// prefix. They identify parent and child rows respectively and may contain
// several columns to form a composite key. NULL key values are encoded
// separately from zero values, so a NULL key never collapses onto a key of
// 0 (or "", false, ...).
//
// Column to field mapping always goes through the existing reflectx Mapper;
// result columns are looked up by name, so reordering the SELECT list does
// not change the result.
type GroupConfig struct {
	// ParentPrefix names the result columns mapped onto the parent struct.
	ParentPrefix string
	// ChildPrefix names the result columns mapped onto the child struct.
	ChildPrefix string
	// ParentKeys lists the full result column names forming the parent key.
	// Each column must use ParentPrefix and occur in the result.
	ParentKeys []string
	// ChildKeys lists the full result column names forming the child key.
	// Each column must use ChildPrefix and occur in the result.
	ChildKeys []string
}

// GroupRowError reports a failure on a specific 1-based data row while
// grouping rows. Use errors.As to recover the row number.
type GroupRowError struct {
	Row int
	Err error
}

func (e *GroupRowError) Error() string {
	return fmt.Sprintf("sqlx: group scan failed at row %d: %v", e.Row, e.Err)
}

// Unwrap returns the underlying error.
func (e *GroupRowError) Unwrap() error { return e.Err }

// groupColumn describes how one result column is consumed.
type groupColumn struct {
	name   string
	child  bool
	keyIdx int // index within ParentKeys/ChildKeys, or -1
	fields []int
}

// ScanGroups consumes a JOIN result and groups the repeated rows into parent
// structs containing lists of child structs.
//
// dest must be a pointer to a slice of parent structs, or a pointer to a
// slice of pointers to parent structs. The parent struct must contain exactly
// one exported field whose (dereferenced) type is a slice of structs; that
// field receives the children, and its element type is the child struct type.
// The child slice may use value or pointer elements.
//
// First-seen order is preserved for both parents and children. A child key
// repeated with identical content is de-duplicated; the same key with any
// conflicting column value returns an error, as does a repeated parent key
// with conflicting parent columns. A LEFT JOIN row whose child columns are
// all NULL creates no child object while still keeping the parent.
//
// On any failure (bad configuration, an unknown or unmapped column, a key
// conflict, a Scan error or Rows.Err) dest is left unchanged: no partial
// result is written. Failures while iterating rows return a *GroupRowError
// carrying the 1-based row number.
//
// ScanGroups never closes rows. The caller owns rows and is responsible for
// closing it, typically with "defer rows.Close()" immediately after a
// successful query.
func ScanGroups(rows rowsi, dest interface{}, cfg GroupConfig) error {
	if strings.TrimSpace(cfg.ParentPrefix) == "" {
		return errors.New("sqlx: GroupConfig.ParentPrefix must be set")
	}
	if strings.TrimSpace(cfg.ChildPrefix) == "" {
		return errors.New("sqlx: GroupConfig.ChildPrefix must be set")
	}
	if cfg.ParentPrefix == cfg.ChildPrefix {
		return errors.New("sqlx: GroupConfig.ParentPrefix and ChildPrefix must differ")
	}
	if len(cfg.ParentKeys) == 0 {
		return errors.New("sqlx: GroupConfig.ParentKeys must list at least one column")
	}
	if len(cfg.ChildKeys) == 0 {
		return errors.New("sqlx: GroupConfig.ChildKeys must list at least one column")
	}

	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return errors.New("must pass a non-nil pointer to ScanGroups destination")
	}
	direct := v.Elem()
	sliceType, err := baseType(direct.Type(), reflect.Slice)
	if err != nil {
		return err
	}
	parentPtr := sliceType.Elem().Kind() == reflect.Ptr
	parentBase := reflectx.Deref(sliceType.Elem())
	if parentBase.Kind() != reflect.Struct {
		return fmt.Errorf("ScanGroups destination must be a slice of structs, got %s", sliceType)
	}

	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	if len(columns) == 0 {
		return errors.New("sqlx: ScanGroups requires at least one result column")
	}

	m := mapper()
	if r, ok := rows.(*Rows); ok && r.Mapper != nil {
		m = r.Mapper
	}

	childField, childSliceType, childBase, childPtr, err := findChildSlice(m, parentBase)
	if err != nil {
		return err
	}

	plans := make([]groupColumn, len(columns))
	parentNames := make([]string, 0, len(columns))
	childNames := make([]string, 0, len(columns))
	columnIndex := make(map[string]int, len(columns))
	for i, column := range columns {
		if _, dup := columnIndex[column]; dup {
			return fmt.Errorf("sqlx: duplicate result column %q", column)
		}
		columnIndex[column] = i

		asParent, parentOK := stripColumnPrefix(column, cfg.ParentPrefix)
		asChild, childOK := stripColumnPrefix(column, cfg.ChildPrefix)
		if parentOK == childOK {
			return fmt.Errorf("sqlx: column %q must match exactly one of parent prefix %q or child prefix %q", column, cfg.ParentPrefix, cfg.ChildPrefix)
		}
		plans[i] = groupColumn{name: column, child: childOK, keyIdx: -1}
		if childOK {
			childNames = append(childNames, asChild)
		} else {
			parentNames = append(parentNames, asParent)
		}
	}

	if err := validateKeyColumns(cfg.ParentKeys, columnIndex, cfg, false); err != nil {
		return err
	}
	if err := validateKeyColumns(cfg.ChildKeys, columnIndex, cfg, true); err != nil {
		return err
	}

	parentTraversals := m.TraversalsByName(parentBase, parentNames)
	childTraversals := m.TraversalsByName(childBase, childNames)
	seenParentFields := make(map[string]string)
	seenChildFields := make(map[string]string)
	pIdx, cIdx := 0, 0
	for i := range plans {
		p := &plans[i]
		var fieldName string
		var traversal []int
		if p.child {
			fieldName, _ = stripColumnPrefix(p.name, cfg.ChildPrefix)
			traversal = childTraversals[cIdx]
			if ki, ok := keyPosition(cfg.ChildKeys, p.name); ok {
				p.keyIdx = ki
			}
			cIdx++
		} else {
			fieldName, _ = stripColumnPrefix(p.name, cfg.ParentPrefix)
			traversal = parentTraversals[pIdx]
			if ki, ok := keyPosition(cfg.ParentKeys, p.name); ok {
				p.keyIdx = ki
			}
			pIdx++
		}
		if len(traversal) == 0 {
			what := "parent"
			if p.child {
				what = "child"
			}
			return fmt.Errorf("missing destination name %q (column %q) in %s struct", fieldName, p.name, what)
		}
		p.fields = traversal
		seen := seenParentFields
		if p.child {
			seen = seenChildFields
		}
		path := fmt.Sprint(traversal)
		if other, dup := seen[path]; dup {
			return fmt.Errorf("sqlx: columns %q and %q map to the same struct field", other, p.name)
		}
		seen[path] = p.name
	}

	raw := make([]interface{}, len(columns))
	values := make([]interface{}, len(columns))
	result := reflect.MakeSlice(sliceType, 0, 0)
	parents := make(map[string]reflect.Value)
	parentSnapshots := make(map[string]reflect.Value)
	parentOrder := make([]string, 0)
	children := make(map[string]map[string]reflect.Value)

	rowNum := 0
	for rows.Next() {
		rowNum++
		for i := range raw {
			raw[i] = new(interface{})
		}
		if err := rows.Scan(raw...); err != nil {
			return &GroupRowError{Row: rowNum, Err: err}
		}

		allChildNull := true
		for i := range columns {
			if plans[i].child && *(raw[i].(*interface{})) != nil {
				allChildNull = false
				break
			}
		}

		parentKey := encodeKey(raw, plans, false)
		candidateParent := reflect.New(parentBase).Elem()
		reflectx.FieldByIndexes(candidateParent, childField).Set(reflect.MakeSlice(childSliceType, 0, 0))
		candidateChild := reflect.Value{}
		if !allChildNull {
			candidateChild = reflect.New(childBase).Elem()
		}

		for i := range columns {
			p := plans[i]
			if p.child && allChildNull {
				// NULL cannot scan into plain value fields; discard it.
				values[i] = raw[i]
				continue
			}
			target := candidateParent
			if p.child {
				target = candidateChild
			}
			values[i] = reflectx.FieldByIndexes(target, p.fields).Addr().Interface()
		}
		if err := rows.Scan(values...); err != nil {
			return &GroupRowError{Row: rowNum, Err: err}
		}

		if live, ok := parents[parentKey]; ok {
			if !reflect.DeepEqual(candidateParent.Interface(), parentSnapshots[parentKey].Interface()) {
				return &GroupRowError{Row: rowNum, Err: fmt.Errorf("parent key %s has conflicting column values", parentKey)}
			}
			candidateParent = live
		} else {
			snapshot := reflect.New(parentBase).Elem()
			snapshot.Set(candidateParent)
			parents[parentKey] = candidateParent
			parentSnapshots[parentKey] = snapshot
			parentOrder = append(parentOrder, parentKey)
			children[parentKey] = make(map[string]reflect.Value)
		}

		if !allChildNull {
			childKey := encodeKey(raw, plans, true)
			known := children[parentKey]
			if snapshot, ok := known[childKey]; ok {
				if !reflect.DeepEqual(candidateChild.Interface(), snapshot.Interface()) {
					return &GroupRowError{Row: rowNum, Err: fmt.Errorf("child key %s under parent key %s has conflicting column values", childKey, parentKey)}
				}
			} else {
				snapshot := reflect.New(childBase).Elem()
				snapshot.Set(candidateChild)
				known[childKey] = snapshot
				childSlice := reflectx.FieldByIndexes(candidateParent, childField)
				if childPtr {
					cp := reflect.New(childBase)
					cp.Elem().Set(candidateChild)
					childSlice.Set(reflect.Append(childSlice, cp))
				} else {
					childSlice.Set(reflect.Append(childSlice, candidateChild))
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		if rowNum == 0 {
			return err
		}
		return &GroupRowError{Row: rowNum, Err: err}
	}

	for _, key := range parentOrder {
		if parentPtr {
			p := reflect.New(parentBase)
			p.Elem().Set(parents[key])
			result = reflect.Append(result, p)
		} else {
			result = reflect.Append(result, parents[key])
		}
	}
	direct.Set(result)
	return nil
}

// ScanGroups on *Rows groups the JOIN result using the Rows' Mapper. See the
// package-level ScanGroups documentation; as there, the caller must Close the
// Rows when done.
func (r *Rows) ScanGroups(dest interface{}, cfg GroupConfig) error {
	return ScanGroups(r, dest, cfg)
}

// findChildSlice locates the unique exported slice-of-structs field on the
// parent struct and returns its traversal, slice type, element type and
// whether the slice stores pointers.
func findChildSlice(m *reflectx.Mapper, parent reflect.Type) ([]int, reflect.Type, reflect.Type, bool, error) {
	var found []int
	var sliceType reflect.Type
	for _, fi := range m.TypeMap(parent).Index {
		t := fi.Field.Type
		if t.Kind() != reflect.Slice {
			continue
		}
		if reflectx.Deref(t.Elem()).Kind() != reflect.Struct {
			continue
		}
		if found != nil {
			return nil, nil, nil, false, fmt.Errorf("sqlx: parent struct %s has multiple slice-of-struct fields; exactly one is required as the child target", parent)
		}
		found = fi.Index
		sliceType = t
	}
	if found == nil {
		return nil, nil, nil, false, fmt.Errorf("sqlx: parent struct %s has no slice-of-struct field to receive children", parent)
	}
	return found, sliceType, reflectx.Deref(sliceType.Elem()), sliceType.Elem().Kind() == reflect.Ptr, nil
}

// stripColumnPrefix returns the part of column following prefix and a dot.
// A column equal to the prefix itself is not accepted.
func stripColumnPrefix(column, prefix string) (string, bool) {
	if strings.HasPrefix(column, prefix+".") {
		return column[len(prefix)+1:], true
	}
	return "", false
}

// validateKeyColumns ensures every configured key column exists in the result,
// is listed only once and uses the required prefix.
func validateKeyColumns(keys []string, columns map[string]int, cfg GroupConfig, child bool) error {
	kind := "ParentKeys"
	prefix := cfg.ParentPrefix
	if child {
		kind = "ChildKeys"
		prefix = cfg.ChildPrefix
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			return fmt.Errorf("sqlx: %s lists column %q more than once", kind, key)
		}
		seen[key] = true
		if _, ok := columns[key]; !ok {
			return fmt.Errorf("sqlx: %s column %q is not in the result", kind, key)
		}
		fieldName, ok := stripColumnPrefix(key, prefix)
		if !ok || fieldName == "" {
			return fmt.Errorf("sqlx: %s column %q must use prefix %q", kind, key, prefix)
		}
	}
	return nil
}

// keyPosition returns the index of column in keys.
func keyPosition(keys []string, column string) (int, bool) {
	for i, key := range keys {
		if key == column {
			return i, true
		}
	}
	return -1, false
}

// keyPart collects raw key values in the configured key-column order.
type keyPart struct {
	keyIdx int
	value  interface{}
}

// encodeKey builds a stable string identity for the parent or child key of a
// row. The encoding distinguishes a NULL value from every non-NULL value,
// including zero values and empty strings. Raw scan values are the values
// produced by database/sql for an *interface{} destination (nil, []byte,
// int64, float64, bool and time.Time).
func encodeKey(raw []interface{}, plans []groupColumn, child bool) string {
	var parts []keyPart
	for i, p := range plans {
		if p.child != child || p.keyIdx < 0 {
			continue
		}
		parts = append(parts, keyPart{keyIdx: p.keyIdx, value: *(raw[i].(*interface{}))})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].keyIdx < parts[j].keyIdx })

	var b strings.Builder
	for i, part := range parts {
		if i > 0 {
			b.WriteByte('|')
		}
		switch value := part.value.(type) {
		case nil:
			b.WriteString("null")
		case []byte:
			b.WriteString("bytes:")
			b.WriteString(strconv.Quote(string(value)))
		case string:
			b.WriteString("string:")
			b.WriteString(strconv.Quote(value))
		case int64:
			b.WriteString("int64:")
			b.WriteString(strconv.FormatInt(value, 10))
		case float64:
			b.WriteString("float64:")
			b.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
		case bool:
			b.WriteString("bool:")
			b.WriteString(strconv.FormatBool(value))
		default:
			b.WriteString(reflect.TypeOf(value).String())
			b.WriteByte(':')
			b.WriteString(strconv.Quote(fmt.Sprintf("%v", value)))
		}
	}
	return b.String()
}
