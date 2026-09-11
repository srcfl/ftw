package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Ask why conversations live on the box, not in a browser: the box is the
// record, and an operator who asked from a laptop should find the answer
// from a phone.
//
// A house keeps a small number of these. The cap exists because the box
// is usually a Raspberry Pi on an SD card, where unbounded append is a
// wear problem long before it is a space problem.
const (
	// AssistantThreadCap is how many conversations the box keeps.
	AssistantThreadCap = 50
	// assistantTitleRunes caps the title taken from the first question.
	assistantTitleRunes = 80
	// assistantThreadRunes caps one stored conversation. A runaway model
	// must not be able to fill the card.
	assistantThreadRunes = 60_000
)

// AssistantTurn is one line of an Ask why conversation.
type AssistantTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
	TsMs int64  `json:"ts_ms,omitempty"`
}

// AssistantThread is one conversation. Turns is empty in list results.
type AssistantThread struct {
	ID        string          `json:"id"`
	StartedMs int64           `json:"started_ms"`
	UpdatedMs int64           `json:"updated_ms"`
	Title     string          `json:"title"`
	Model     string          `json:"model,omitempty"`
	TurnCount int             `json:"turn_count"`
	Turns     []AssistantTurn `json:"turns,omitempty"`
}

// SaveAssistantThread writes a conversation and prunes the oldest beyond
// AssistantThreadCap. The id is chosen by the caller so a reply can name
// the thread it belongs to.
func (s *Store) SaveAssistantThread(t AssistantThread) error {
	id := strings.TrimSpace(t.ID)
	if id == "" {
		return fmt.Errorf("assistant thread: empty id")
	}
	now := time.Now().UnixMilli()
	if t.StartedMs == 0 {
		t.StartedMs = now
	}
	t.UpdatedMs = now
	t.Title = assistantTitle(t.Title, t.Turns)

	raw, err := json.Marshal(trimAssistantTurns(t.Turns))
	if err != nil {
		return fmt.Errorf("assistant thread marshal: %w", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO assistant_threads (id, started_ms, updated_ms, title, model, turns_json)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				updated_ms = excluded.updated_ms,
				title      = excluded.title,
				model      = excluded.model,
				turns_json = excluded.turns_json`,
		id, t.StartedMs, t.UpdatedMs, t.Title, t.Model, string(raw)); err != nil {
		return fmt.Errorf("assistant_threads insert: %w", err)
	}
	// Keep the newest AssistantThreadCap rows. Ties on updated_ms break on
	// id so the delete is deterministic.
	if _, err := s.db.Exec(
		`DELETE FROM assistant_threads WHERE id NOT IN (
			SELECT id FROM assistant_threads ORDER BY updated_ms DESC, id DESC LIMIT ?
		)`, AssistantThreadCap); err != nil {
		return fmt.Errorf("assistant_threads prune: %w", err)
	}
	return nil
}

// RecentAssistantThreads lists conversations newest first, without turns.
func (s *Store) RecentAssistantThreads(limit int) ([]AssistantThread, error) {
	if limit <= 0 || limit > AssistantThreadCap {
		limit = AssistantThreadCap
	}
	rows, err := s.db.Query(
		`SELECT id, started_ms, updated_ms, title, model, turns_json
			FROM assistant_threads ORDER BY updated_ms DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AssistantThread, 0, limit)
	for rows.Next() {
		var t AssistantThread
		var raw string
		if err := rows.Scan(&t.ID, &t.StartedMs, &t.UpdatedMs, &t.Title, &t.Model, &raw); err != nil {
			return out, err
		}
		var turns []AssistantTurn
		if err := json.Unmarshal([]byte(raw), &turns); err == nil {
			t.TurnCount = len(turns)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AssistantThreadByID returns one conversation with its turns. The bool is
// false when no such thread exists.
func (s *Store) AssistantThreadByID(id string) (AssistantThread, bool, error) {
	var t AssistantThread
	var raw string
	err := s.db.QueryRow(
		`SELECT id, started_ms, updated_ms, title, model, turns_json
			FROM assistant_threads WHERE id = ?`, strings.TrimSpace(id)).
		Scan(&t.ID, &t.StartedMs, &t.UpdatedMs, &t.Title, &t.Model, &raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AssistantThread{}, false, nil
		}
		return AssistantThread{}, false, err
	}
	if err := json.Unmarshal([]byte(raw), &t.Turns); err != nil {
		t.Turns = nil
	}
	t.TurnCount = len(t.Turns)
	return t, true, nil
}

// DeleteAssistantThread removes one conversation. Deleting a thread that
// is already gone is not an error.
func (s *Store) DeleteAssistantThread(id string) error {
	_, err := s.db.Exec(`DELETE FROM assistant_threads WHERE id = ?`, strings.TrimSpace(id))
	return err
}

// DeleteAllAssistantThreads clears the history.
func (s *Store) DeleteAllAssistantThreads() error {
	_, err := s.db.Exec(`DELETE FROM assistant_threads`)
	return err
}

// assistantTitle names a thread by its first question.
func assistantTitle(given string, turns []AssistantTurn) string {
	title := strings.TrimSpace(given)
	if title == "" {
		for _, t := range turns {
			if t.Role == "user" && strings.TrimSpace(t.Text) != "" {
				title = strings.TrimSpace(t.Text)
				break
			}
		}
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return "Ask why"
	}
	if utf8.RuneCountInString(title) > assistantTitleRunes {
		title = string([]rune(title)[:assistantTitleRunes-1]) + "…"
	}
	return title
}

// trimAssistantTurns drops the oldest turns until the conversation fits.
func trimAssistantTurns(turns []AssistantTurn) []AssistantTurn {
	total := 0
	for _, t := range turns {
		total += utf8.RuneCountInString(t.Text)
	}
	for len(turns) > 1 && total > assistantThreadRunes {
		total -= utf8.RuneCountInString(turns[0].Text)
		turns = turns[1:]
	}
	if len(turns) == 1 && utf8.RuneCountInString(turns[0].Text) > assistantThreadRunes {
		turns[0].Text = string([]rune(turns[0].Text)[:assistantThreadRunes]) + "…"
	}
	return turns
}
