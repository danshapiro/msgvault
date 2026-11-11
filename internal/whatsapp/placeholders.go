package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func quoteSQLiteName(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func sourceColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+quoteSQLiteName(table)+")")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var value sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notnull, &value, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

// A single reserved row is used as a baseline by chat read/important pointers.
// Every other orphan remains unresolved. Unknown non-NULL message columns or
// related payloads prevent this exclusion, including in newer source schemas.
func countReservedPlaceholder(ctx context.Context, db *sql.DB) (int64, error) {
	cols, err := sourceColumns(ctx, db, "message")
	if err != nil {
		return 0, err
	}
	known := map[string]bool{"_id": true, "chat_row_id": true, "from_me": true, "key_id": true, "sort_id": true}
	foundSort := false
	checks := []string{"_id=1", "chat_row_id=-1", "key_id='-1'", "from_me=0", "NOT EXISTS (SELECT 1 FROM chat WHERE _id=-1)"}
	for _, name := range cols {
		if name == "sort_id" {
			foundSort = true
			checks = append(checks, "sort_id=0")
		} else if !known[name] {
			checks = append(checks, quoteSQLiteName(name)+" IS NULL")
		}
	}
	if !foundSort {
		return 0, nil
	}
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM message WHERE "+strings.Join(checks, " AND ")).Scan(&count); err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT IN ('message','chat')`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return 0, err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return 0, err
	}
	if closeErr != nil {
		return 0, closeErr
	}
	for _, table := range tables {
		cols, err := sourceColumns(ctx, db, table)
		if err != nil {
			return 0, err
		}
		for _, col := range cols {
			if col != "message_table_id" && !strings.HasSuffix(col, "message_row_id") {
				continue
			}
			var payload bool
			if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+quoteSQLiteName(table)+" WHERE "+quoteSQLiteName(col)+"=1)").Scan(&payload); err != nil {
				return 0, fmt.Errorf("check reserved message payload: %w", err)
			}
			if payload {
				return 0, nil
			}
		}
	}
	return count, nil
}
