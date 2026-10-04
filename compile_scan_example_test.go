package sqlx_test

import (
	"fmt"
	"log"

	"github.com/jmoiron/sqlx"
)

// membershipRow is the destination for a user/team join whose columns are
// [id, id, name]. It keeps an "id" field, so both joined id columns resolve
// to the same destination under the default rules.
type membershipRow struct {
	ID     int64  `db:"id"`
	UserID int64  `db:"user_id"`
	TeamID int64  `db:"team_id"`
	Name   string `db:"name"`
}

// ExampleCompileStructScan demonstrates compiling the column mapping once,
// printing it, and then scanning join rows whose first two columns share the
// name "id" into distinct fields.
func ExampleCompileStructScan() {
	// the in-memory driver is registered by the test fixtures; in real code
	// use db.Queryx against your database
	db, err := sqlx.Open("sqlx-mem", "")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// 1) strict compilation: [id, id, name] fails by default and names the
	//    conflicting column positions.
	if _, err := sqlx.CompileStructScan((*membershipRow)(nil),
		[]string{"id", "id", "name"}); err != nil {
		fmt.Println("strict:", err)
	}

	// 2) resolve it by assigning each column ordinal explicitly.
	plan, err := sqlx.CompileStructScan((*membershipRow)(nil),
		[]string{"id", "id", "name"},
		sqlx.WithColumnFields(
			sqlx.ColumnField{Column: 0, Field: "user_id"},
			sqlx.ColumnField{Column: 1, Field: "team_id"},
		),
	)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("mapping:")
	for _, cm := range plan.Columns() {
		fmt.Printf("  column %d %-4q -> %s\n", cm.Column, cm.Name, cm.Field)
	}

	rows, err := db.Queryx("mem:id,id,name|1:7:alice")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var m membershipRow
		if err := plan.Scan(rows, &m); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("result: user=%d team=%d name=%s\n", m.UserID, m.TeamID, m.Name)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	// Output:
	// strict: columns 0 ("id") and 1 ("id") both map to destination field "id" in sqlx_test.membershipRow; use WithColumnFields to assign each column ordinal explicitly
	// mapping:
	//   column 0 "id" -> user_id
	//   column 1 "id" -> team_id
	//   column 2 "name" -> name
	// result: user=1 team=7 name=alice
}
