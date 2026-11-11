package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// waLIDLabel is observed source presentation, not a phone/person association.
// Source labels have no per-label timestamp; a changed label describes the
// snapshot being imported, not a verified current real-world identity.
type waLIDLabel struct {
	Username    string
	DisplayName string
}

func fetchLIDLabels(ctx context.Context, db *sql.DB) (map[string]waLIDLabel, error) {
	labels := make(map[string]waLIDLabel)
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='lid_display_name')`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("inspect WhatsApp source labels: %w", err)
	}
	if !exists {
		return labels, nil
	}
	columns, err := lidLabelColumns(ctx, db)
	if err != nil {
		return nil, err
	}
	if !columns["lid_row_id"] || !columns["display_name"] {
		return nil, errors.New("WhatsApp source labels lack required identity/display columns")
	}
	username := "''"
	if columns["username"] {
		username = "COALESCE(n.username,'')"
	}
	rows, err := db.QueryContext(ctx, `SELECT j.raw_string,COALESCE(n.display_name,''),`+username+` FROM lid_display_name n JOIN jid j ON j._id=n.lid_row_id WHERE j.server='lid'`)
	if err != nil {
		return nil, fmt.Errorf("read WhatsApp source labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var jid string
		var label waLIDLabel
		if err := rows.Scan(&jid, &label.DisplayName, &label.Username); err != nil {
			return nil, fmt.Errorf("read WhatsApp source label: %w", err)
		}
		label.Username = strings.TrimPrefix(strings.TrimSpace(label.Username), "@")
		label.DisplayName = strings.TrimSpace(label.DisplayName)
		if previous, ok := labels[jid]; ok && previous != label {
			return nil, errors.New("WhatsApp source LID has conflicting display labels")
		}
		labels[jid] = label
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate WhatsApp source labels: %w", err)
	}
	return labels, nil
}

func lidLabelColumns(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(lid_display_name)`)
	if err != nil {
		return nil, fmt.Errorf("inspect WhatsApp source label columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			return nil, fmt.Errorf("read WhatsApp source label columns: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read WhatsApp source label columns: %w", err)
	}
	return columns, nil
}
