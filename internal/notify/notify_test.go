package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// The contract: POST $NOTIFY_URL/v1/events with the bearer token and the event as JSON; a failure is tried again.
func TestClient_SendsTheContract(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/events" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	n := New(ctx, srv.URL+"/", "secret", zap.NewNop().Sugar())
	c := n.(*Client)
	c.backoff = time.Millisecond
	if !n.Enabled() {
		t.Fatal("a client with a URL is enabled")
	}
	n.Notify(Event{Type: TypeAllocationShortened, Key: "allocation-shortened:a_1", Class: ClassAction, Recipients: []Recipient{{User: "a@b.de"}, {Group: "course"}},
		Actor: "a@b.de", Due: "2027-01-01T00:00:00+01:00", Object: Object{Kind: "project", ID: "p_1", Name: "P", URL: "https://portal/x"}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	c.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || calls != 2 {
		t.Fatalf("delivered %d after %d calls", len(got), calls)
	}
	ev := got[0]
	if ev["source"] != Source || ev["type"] != TypeAllocationShortened || ev["key"] != "allocation-shortened:a_1" || ev["class"] != "action" || ev["resolve"] != false || ev["actor"] != "a@b.de" || ev["due"] == nil {
		t.Errorf("event = %v", ev)
	}
	if obj := ev["object"].(map[string]any); obj["kind"] != "project" || obj["id"] != "p_1" || obj["url"] != "https://portal/x" {
		t.Errorf("object = %v", obj)
	}
	if rs := ev["recipients"].([]any); len(rs) != 2 || rs[1].(map[string]any)["group"] != "course" {
		t.Errorf("recipients = %v", rs)
	}
	if _, ok := ev["data"].(map[string]any); !ok {
		t.Errorf("data is an object: %v", ev["data"])
	}
}

// Without a URL nothing is sent and nothing may be cut.
func TestNew_WithoutURLIsNop(t *testing.T) {
	n := New(context.Background(), " ", "x", zap.NewNop().Sugar())
	if n.Enabled() {
		t.Error("without NOTIFY_URL notifications are off")
	}
	n.Notify(Event{})
}
