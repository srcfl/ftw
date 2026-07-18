package state

// ComponentUpdate is an append-safe audit record for independently updated
// core, optimizer and driver components. OperationKey is stable across core
// recreation so the post-update process can finish the row started by the
// pre-update process.
type ComponentUpdate struct {
	ID           int64  `json:"id"`
	OperationKey string `json:"operation_key"`
	Kind         string `json:"kind"`
	ComponentID  string `json:"component_id"`
	Action       string `json:"action"`
	FromVersion  string `json:"from_version,omitempty"`
	ToVersion    string `json:"to_version,omitempty"`
	Outcome      string `json:"outcome"`
	Message      string `json:"message,omitempty"`
	StartedAtMS  int64  `json:"started_at_ms"`
	FinishedAtMS int64  `json:"finished_at_ms,omitempty"`
}

func (s *Store) UpsertComponentUpdate(in ComponentUpdate) (ComponentUpdate, error) {
	_, err := s.db.Exec(`INSERT INTO component_updates
		(operation_key, kind, component_id, action, from_version, to_version, outcome, message, started_at_ms, finished_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(operation_key) DO UPDATE SET
			kind=excluded.kind,
			component_id=excluded.component_id,
			action=excluded.action,
			from_version=CASE WHEN excluded.from_version = '' THEN component_updates.from_version ELSE excluded.from_version END,
			to_version=CASE WHEN excluded.to_version = '' THEN component_updates.to_version ELSE excluded.to_version END,
			outcome=excluded.outcome,
			message=excluded.message,
			finished_at_ms=excluded.finished_at_ms`,
		in.OperationKey, in.Kind, in.ComponentID, in.Action, in.FromVersion, in.ToVersion,
		in.Outcome, in.Message, in.StartedAtMS, in.FinishedAtMS)
	if err != nil {
		return ComponentUpdate{}, err
	}
	return s.ComponentUpdateByKey(in.OperationKey)
}

func (s *Store) ComponentUpdateByKey(key string) (ComponentUpdate, error) {
	return scanComponentUpdate(s.db.QueryRow(`SELECT id, operation_key, kind, component_id, action,
		from_version, to_version, outcome, message, started_at_ms, finished_at_ms
		FROM component_updates WHERE operation_key = ?`, key))
}

func (s *Store) ComponentUpdates(kind, componentID string, limit int) ([]ComponentUpdate, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, operation_key, kind, component_id, action,
		from_version, to_version, outcome, message, started_at_ms, finished_at_ms
		FROM component_updates
		WHERE (? = '' OR kind = ?) AND (? = '' OR component_id = ?)
		ORDER BY started_at_ms DESC, id DESC LIMIT ?`, kind, kind, componentID, componentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ComponentUpdate
	for rows.Next() {
		event, err := scanComponentUpdate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

type componentUpdateScanner interface{ Scan(...any) error }

func scanComponentUpdate(row componentUpdateScanner) (ComponentUpdate, error) {
	var out ComponentUpdate
	err := row.Scan(&out.ID, &out.OperationKey, &out.Kind, &out.ComponentID, &out.Action,
		&out.FromVersion, &out.ToVersion, &out.Outcome, &out.Message, &out.StartedAtMS, &out.FinishedAtMS)
	return out, err
}
