package state

import (
	"fmt"
	"strings"
	"testing"
)

func askThread(id, question, answer string) AssistantThread {
	return AssistantThread{
		ID: id,
		Turns: []AssistantTurn{
			{Role: "user", Text: question},
			{Role: "assistant", Text: answer},
		},
	}
}

func TestAssistantThreadRoundTrips(t *testing.T) {
	s := openTestStore(t)
	if err := s.SaveAssistantThread(askThread("aaaa000000000001", "why is it charging?", "cheap slot")); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.AssistantThreadByID("aaaa000000000001")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.Title != "why is it charging?" {
		t.Fatalf("title = %q, want the first question", got.Title)
	}
	if len(got.Turns) != 2 || got.Turns[1].Text != "cheap slot" {
		t.Fatalf("turns = %#v", got.Turns)
	}
	if got.UpdatedMs == 0 || got.StartedMs == 0 {
		t.Fatalf("timestamps not set: %+v", got)
	}
}

func TestAssistantThreadMissingIsNotAnError(t *testing.T) {
	s := openTestStore(t)
	_, ok, err := s.AssistantThreadByID("ffff000000000000")
	if err != nil {
		t.Fatalf("err = %v, want nil for a missing thread", err)
	}
	if ok {
		t.Fatal("ok = true for a thread that was never written")
	}
}

// A follow-up must land in the same row, not start a second one.
func TestAssistantThreadFollowUpUpdatesInPlace(t *testing.T) {
	s := openTestStore(t)
	id := "bbbb000000000001"
	if err := s.SaveAssistantThread(askThread(id, "first", "one")); err != nil {
		t.Fatal(err)
	}
	grown := askThread(id, "first", "one")
	grown.Turns = append(grown.Turns,
		AssistantTurn{Role: "user", Text: "second"},
		AssistantTurn{Role: "assistant", Text: "two"})
	if err := s.SaveAssistantThread(grown); err != nil {
		t.Fatal(err)
	}
	list, err := s.RecentAssistantThreads(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("threads = %d, want 1 row after a follow-up", len(list))
	}
	if list[0].TurnCount != 4 {
		t.Fatalf("turn count = %d, want 4", list[0].TurnCount)
	}
	if list[0].Title != "first" {
		t.Fatalf("title = %q, want the first question to stick", list[0].Title)
	}
}

// The box is usually a Pi on an SD card. History cannot grow forever.
func TestAssistantThreadsAreCappedNewestFirst(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < AssistantThreadCap+7; i++ {
		id := fmt.Sprintf("cccc%012d", i)
		if err := s.SaveAssistantThread(askThread(id, fmt.Sprintf("question %d", i), "answer")); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.RecentAssistantThreads(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != AssistantThreadCap {
		t.Fatalf("kept %d threads, want the cap of %d", len(list), AssistantThreadCap)
	}
	// The oldest seven are the ones that went.
	for _, th := range list {
		if th.Title == "question 0" {
			t.Fatal("the oldest thread survived the cap")
		}
	}
}

func TestAssistantThreadDeletes(t *testing.T) {
	s := openTestStore(t)
	if err := s.SaveAssistantThread(askThread("dddd000000000001", "q", "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAssistantThread("dddd000000000001"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AssistantThreadByID("dddd000000000001"); ok {
		t.Fatal("thread survived delete")
	}
	// Deleting again is not an error.
	if err := s.DeleteAssistantThread("dddd000000000001"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestAssistantThreadsClear(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.SaveAssistantThread(askThread(fmt.Sprintf("eeee%012d", i), "q", "a")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteAllAssistantThreads(); err != nil {
		t.Fatal(err)
	}
	list, err := s.RecentAssistantThreads(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("threads = %d after clear, want 0", len(list))
	}
}

// A model that will not stop writing must not fill the card.
func TestAssistantThreadTrimsARunawayAnswer(t *testing.T) {
	s := openTestStore(t)
	huge := strings.Repeat("x", assistantThreadRunes*2)
	if err := s.SaveAssistantThread(askThread("aaaa000000000009", "q", huge)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.AssistantThreadByID("aaaa000000000009")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	total := 0
	for _, turn := range got.Turns {
		total += len([]rune(turn.Text))
	}
	if total > assistantThreadRunes+8 {
		t.Fatalf("stored %d runes, want the thread capped near %d", total, assistantThreadRunes)
	}
}

func TestAssistantThreadTitleFallsBackWhenNoQuestion(t *testing.T) {
	got := assistantTitle("", []AssistantTurn{{Role: "assistant", Text: "only an answer"}})
	if got != "Ask why" {
		t.Fatalf("title = %q, want the fallback", got)
	}
}

func TestAssistantThreadTitleCollapsesWhitespace(t *testing.T) {
	got := assistantTitle("  why   is\nit\tcharging?  ", nil)
	if got != "why is it charging?" {
		t.Fatalf("title = %q", got)
	}
}

func TestAssistantThreadRejectsEmptyID(t *testing.T) {
	s := openTestStore(t)
	if err := s.SaveAssistantThread(askThread("", "q", "a")); err == nil {
		t.Fatal("an empty id was accepted")
	}
}
