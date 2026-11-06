package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// The file hash changes when corrections are published. This logical digest
// still checks every original table and unrelated estimate on a retry, while
// allowing only this plan's exact rows and recomputable daily cache contents.
func repairHistoryEvidence(ctx context.Context, db *sql.DB, minutes []repairMinute, method string) (string, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	h := sha256.New()
	enc := json.NewEncoder(h)
	schema, err := tx.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`)
	if err != nil {
		return "", err
	}
	var tables []string
	for schema.Next() {
		var kind, name, table string
		var statement sql.NullString
		if err = schema.Scan(&kind, &name, &table, &statement); err != nil {
			schema.Close()
			return "", err
		}
		if err = enc.Encode([]any{kind, name, table, statement}); err != nil {
			schema.Close()
			return "", err
		}
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	err = schema.Err()
	schema.Close()
	if err != nil {
		return "", err
	}
	planned := map[int64]float64{}
	for _, m := range minutes {
		planned[m.StartMS] = m.Wh
	}
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	for _, table := range tables {
		switch table {
		case "energy_daily", "energy_daily_partial", "history_daily_quality":
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT * FROM `+quote(table)+` LIMIT 0`)
		if err != nil {
			return "", err
		}
		cols, err := rows.Columns()
		rows.Close()
		if err != nil {
			return "", err
		}
		order := make([]string, 0, len(cols)*2)
		index := map[string]int{}
		for i, c := range cols {
			index[c] = i
			order = append(order, quote(c))
		}
		for _, c := range cols {
			order = append(order, "typeof("+quote(c)+")")
		}
		if err = enc.Encode([]any{table, cols}); err != nil {
			return "", err
		}
		rows, err = tx.QueryContext(ctx, `SELECT * FROM `+quote(table)+` ORDER BY `+strings.Join(order, ","))
		if err != nil {
			return "", err
		}
		for rows.Next() {
			values := make([]any, len(cols))
			targets := make([]any, len(cols))
			for i := range values {
				targets[i] = &values[i]
			}
			if err = rows.Scan(targets...); err != nil {
				rows.Close()
				return "", err
			}
			if table == "history_load_estimates" && method != "" {
				at, atOK := values[index["start_ms"]].(int64)
				wh, exists := planned[at]
				// All planned rows have already been checked exactly by ApplyHistoryRepair.
				if atOK && exists && values[index["resolution_ms"]] == int64(60000) && values[index["covered_ms"]] == int64(60000) && values[index["load_wh"]] == wh && values[index["method"]] == method {
					continue
				}
			}
			typed := make([]any, len(values))
			for i, v := range values {
				typed[i] = []any{fmt.Sprintf("%T", v), v}
			}
			if err = enc.Encode(typed); err != nil {
				rows.Close()
				return "", err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
