package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// assistantHistoryServer is assistantTestServer plus a real store, which is
// what the history needs.
func assistantHistoryServer(t *testing.T, asst *config.Assistant, httpClient *http.Client) (*Server, *state.Store) {
	t.Helper()
	st, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctrl := control.NewState(0, 50, "meter")
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()
	tel.Update("meter", telemetry.DerMeter, 500, nil, nil)
	srv := New(&Deps{
		Ctrl:          ctrl,
		CtrlMu:        &sync.Mutex{},
		Tel:           tel,
		Cfg:           &config.Config{Assistant: asst},
		CfgMu:         &sync.RWMutex{},
		State:         st,
		Version:       "test-version",
		AssistantHTTP: httpClient,
	})
	return srv, st
}

func TestAssistantThreadsAreProtectedReads(t *testing.T) {
	srv, _ := assistantHistoryServer(t, nil, nil)
	for _, path := range []string{
		"/api/assistant/threads",
		"/api/assistant/threads/aaaa000000000001",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if facts := srv.Route(req); facts.Tier != apiauth.TierRead {
			t.Fatalf("%s tier = %v, want Read", path, facts.Tier)
		}
		if !protectedReadPath(path) {
			t.Fatalf("%s is not a protected read — it carries answers about this house", path)
		}
	}
}

func TestAssistantThreadDeleteNeedsConfigure(t *testing.T) {
	srv, _ := assistantHistoryServer(t, nil, nil)
	req := httptest.NewRequest(http.MethodDelete, "/api/assistant/threads/aaaa000000000001", nil)
	if facts := srv.Route(req); facts.Tier != apiauth.TierConfigure {
		t.Fatalf("tier = %v, want Configure", facts.Tier)
	}
}

func TestAssistantThreadsListsNewestFirst(t *testing.T) {
	srv, st := assistantHistoryServer(t, nil, nil)
	for _, id := range []string{"aaaa000000000001", "aaaa000000000002"} {
		if err := st.SaveAssistantThread(state.AssistantThread{
			ID:    id,
			Turns: []state.AssistantTurn{{Role: "user", Text: "q " + id}, {Role: "assistant", Text: "a"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/assistant/threads", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Threads []state.AssistantThread `json:"threads"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Threads) != 2 {
		t.Fatalf("threads = %d, want 2", len(got.Threads))
	}
	if got.Threads[0].ID != "aaaa000000000002" {
		t.Fatalf("first = %q, want the newest", got.Threads[0].ID)
	}
	// The list is an index, not a transcript: turns stay out of it.
	if len(got.Threads[0].Turns) != 0 {
		t.Fatalf("list carried %d turns; it should only carry the count", len(got.Threads[0].Turns))
	}
	if got.Threads[0].TurnCount != 2 {
		t.Fatalf("turn count = %d, want 2", got.Threads[0].TurnCount)
	}
}

func TestAssistantThreadReadsBackTheConversation(t *testing.T) {
	srv, st := assistantHistoryServer(t, nil, nil)
	if err := st.SaveAssistantThread(state.AssistantThread{
		ID:    "bbbb000000000001",
		Model: "qwen/qwen3-8b:free",
		Turns: []state.AssistantTurn{
			{Role: "user", Text: "why is it charging?"},
			{Role: "assistant", Text: "cheap slot"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/assistant/threads/bbbb000000000001", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var got state.AssistantThread
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 2 || got.Turns[0].Text != "why is it charging?" {
		t.Fatalf("turns = %#v", got.Turns)
	}
	if got.Model != "qwen/qwen3-8b:free" {
		t.Fatalf("model = %q", got.Model)
	}
}

func TestAssistantThreadRejectsAStrangeID(t *testing.T) {
	srv, _ := assistantHistoryServer(t, nil, nil)
	for _, id := range []string{"../../etc/passwd", "short", "ZZZZ000000000001", "aaaa00000000000"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/assistant/threads/x", nil)
		req.SetPathValue("id", id)
		srv.handleAssistantThread(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("id %q: status = %d, want 400", id, rr.Code)
		}
	}
}

func TestAssistantThreadMissingIs404(t *testing.T) {
	srv, _ := assistantHistoryServer(t, nil, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/assistant/threads/cccc000000000009", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// The whole point: an answer outlives the dialog that produced it.
func TestAssistantAskStoresTheConversation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "openrouter/free",
			"choices": []map[string]any{
				{"message": map[string]string{
					"role":    "assistant",
					"content": "## Answer\nCheap slot.\n\n## Issue title\n\n## Issue body\n",
				}},
			},
		})
	}))
	defer upstream.Close()

	srv, st := assistantHistoryServer(t, &config.Assistant{
		Enabled: true, APIKey: "k", BaseURL: upstream.URL,
	}, upstream.Client())

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/assistant/ask",
		strings.NewReader(`{"question":"why is it charging?"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleAssistantAsk(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var first assistantAskResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.ThreadID == "" {
		t.Fatal("no thread id came back; the answer was not stored")
	}
	stored, ok, err := st.AssistantThreadByID(first.ThreadID)
	if err != nil || !ok {
		t.Fatalf("thread not stored: ok=%v err=%v", ok, err)
	}
	if len(stored.Turns) != 2 || stored.Turns[0].Text != "why is it charging?" {
		t.Fatalf("turns = %#v", stored.Turns)
	}
	if stored.Turns[1].Text != "Cheap slot." {
		t.Fatalf("stored answer = %q", stored.Turns[1].Text)
	}

	// A follow-up naming that thread grows the same row.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/assistant/ask",
		strings.NewReader(`{"question":"and tonight?","thread_id":`+jsonString(first.ThreadID)+`}`))
	req2.Header.Set("Content-Type", "application/json")
	srv.handleAssistantAsk(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("follow-up status = %d: %s", rr2.Code, rr2.Body.String())
	}
	var second assistantAskResponse
	if err := json.Unmarshal(rr2.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ThreadID != first.ThreadID {
		t.Fatalf("follow-up thread = %q, want %q", second.ThreadID, first.ThreadID)
	}
	grown, _, err := st.AssistantThreadByID(first.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grown.Turns) != 4 {
		t.Fatalf("turns = %d after a follow-up, want 4", len(grown.Turns))
	}
	list, err := st.RecentAssistantThreads(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("threads = %d, want the follow-up to stay in one row", len(list))
	}
}

// A client-supplied id must not be able to name a row of its own choosing.
func TestAssistantAskIgnoresAForgedThreadID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "## Answer\nFine.\n"}},
			},
		})
	}))
	defer upstream.Close()

	srv, st := assistantHistoryServer(t, &config.Assistant{
		Enabled: true, APIKey: "k", BaseURL: upstream.URL,
	}, upstream.Client())

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/assistant/ask",
		strings.NewReader(`{"question":"q","thread_id":"../../etc/passwd"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleAssistantAsk(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var got assistantAskResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !validAssistantThreadID(got.ThreadID) {
		t.Fatalf("thread id = %q, want a fresh generated one", got.ThreadID)
	}
	if _, ok, _ := st.AssistantThreadByID("../../etc/passwd"); ok {
		t.Fatal("the forged id named a row")
	}
}

func TestAssistantThreadsClearEmptiesHistory(t *testing.T) {
	srv, st := assistantHistoryServer(t, nil, nil)
	if err := st.SaveAssistantThread(state.AssistantThread{
		ID:    "dddd000000000001",
		Turns: []state.AssistantTurn{{Role: "user", Text: "q"}, {Role: "assistant", Text: "a"}},
	}); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	srv.handleAssistantThreadsClear(rr, httptest.NewRequest(http.MethodDelete, "/api/assistant/threads", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	list, err := st.RecentAssistantThreads(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("threads = %d after clear, want 0", len(list))
	}
}

// A box with no store still answers; it just cannot remember.
func TestAssistantHistoryWithoutAStoreIsEmptyNotAnError(t *testing.T) {
	srv := assistantTestServer(t, nil, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/assistant/threads", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an empty list", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"threads"`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}
