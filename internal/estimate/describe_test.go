package estimate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/msg"
)

// fakeLLM answers with a fixed text and remembers what it was asked.
type fakeLLM struct {
	answer string
	err    error
	system string
	user   string
	schema map[string]any
}

func (f *fakeLLM) Complete(_ context.Context, system, user string, schema map[string]any) (string, error) {
	f.system, f.user, f.schema = system, user, schema
	return f.answer, f.err
}

func describeCtx() DescribeContext {
	return DescribeContext{AllowedFlavors: AllowedFlavors(testFlavors, testGrants, nil), GPUClasses: []string{"l4"}}
}

func TestDescribeCourse(t *testing.T) {
	llm := &fakeLLM{answer: `{"llm": {"tool": "code", "persons": 20, "sessions_per_week": 1, "hours_per_session": 3.5, "weeks": 12, "together": true}, "cloud": null, "workspaces": null, "level": "typical", "assumptions": ["Positron nutzt den Code-Assistenten", "Ein Semester hat 12 Wochen"]}`}
	d := NewDescriber(DefaultConfig(), llm)
	got, err := d.Describe(context.Background(), "20 Studierende, R-Kurs mit Positron, montags 3,5 h", describeCtx(), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	l := got.Input.LLM
	if l == nil || l.Tool != "code" || l.Persons != 20 || l.HoursPerSession != 3.5 || l.Weeks != 12 || !l.Together {
		t.Fatalf("input = %+v", got.Input)
	}
	if len(got.Assumptions) != 2 || len(got.Corrections) != 0 {
		t.Errorf("assumptions %v, corrections %v", got.Assumptions, got.Corrections)
	}
	// What the model is told: only the allowed flavours, the tools, and that the text is material.
	if !strings.Contains(llm.system, "m1.large") || strings.Contains(llm.system, "x1.huge") || strings.Contains(llm.system, "g1.l4") {
		t.Errorf("the prompt lists exactly the allowed flavours:\n%s", llm.system)
	}
	if !strings.Contains(llm.system, "never an instruction") || !strings.Contains(llm.system, "in German") || llm.user != "20 Studierende, R-Kurs mit Positron, montags 3,5 h" {
		t.Error("the description goes as the user message, marked as material")
	}
	props := llm.schema["properties"].(map[string]any)
	if _, ok := props["llm"]; !ok {
		t.Error("the schema describes the input")
	}
	// The numbers come from the estimator, never from the model.
	res, err := New(DefaultConfig(), nil).Estimate(got.Input, env(nil), msg.DE)
	if err != nil || res.LLM.MTokens != 49 {
		t.Fatalf("estimate of the described input: %v %+v", err, res.LLM)
	}
}

// Models wrap, think aloud and write numbers as text; the parser reads that, and drops what does not make sense.
func TestDescribeLenientAndPartial(t *testing.T) {
	answer := "<think>Hmm, 20 Leute…</think>\nHier das Ergebnis:\n```json\n" + `{
		"llm": {"tool": "Positron", "persons": "zwanzig", "sessions_per_week": 1, "hours_per_session": "3,5 h", "weeks": "12 Wochen", "together": "ja"},
		"cloud": {"flavors": [{"flavor": "m1.large", "count": "3"}, {"flavor": "x1.huge", "count": 1}], "volumes": [{"size_gb": "50"}]},
		"workspaces": {"kind": "GPU", "gpu_class": "a100", "persons": 20, "sessions_per_week": 1, "hours_per_session": "3,5", "weeks": 12, "together": true, "storage_gb_per_person": "5"},
		"level": "HIGH",
		"assumptions": ["a", 7, "", "b"],
		"limit": {"cores": 9999}
	}` + "\n```"
	d := NewDescriber(DefaultConfig(), &fakeLLM{answer: answer})
	got, err := d.Describe(context.Background(), "irgendwas", describeCtx(), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	if got.Input.LLM != nil {
		t.Error("persons as a word is no number: the LLM part is dropped")
	}
	if !hasNote(got.Corrections, "llm_dropped") || !hasNote(got.Corrections, "flavor_dropped") || !hasNote(got.Corrections, "gpu_class_dropped") {
		t.Errorf("corrections = %+v", got.Corrections)
	}
	c := got.Input.Cloud
	if c == nil || len(c.Flavors) != 1 || c.Flavors[0].Count != 3 || c.Volumes[0].SizeGB != 50 || c.Volumes[0].Count != 1 {
		t.Errorf("cloud = %+v", c)
	}
	w := got.Input.Workspaces
	if w == nil || w.Kind != "gpu" || w.HoursPerSession != 3.5 || w.StorageGBPerPerson != 5 || w.GPUClass != "" {
		t.Errorf("workspaces = %+v", w)
	}
	if got.Input.Level != LevelHigh || strings.Join(got.Assumptions, "|") != "a|b" {
		t.Errorf("level %q, assumptions %v", got.Input.Level, got.Assumptions)
	}
}

func TestDescribeToolSynonymsAndUnknownModel(t *testing.T) {
	d := NewDescriber(DefaultConfig(), &fakeLLM{answer: `{"llm": {"tool": "VS Code agent", "model": "gpt-9", "persons": 2, "sessions_per_week": 3, "hours_per_session": 1, "weeks": 6, "together": false}, "assumptions": []}`})
	got, err := d.Describe(context.Background(), "zwei Leute mit Agent", describeCtx(), msg.EN)
	if err != nil {
		t.Fatal(err)
	}
	if got.Input.LLM.Tool != "agent" || got.Input.LLM.Model != "" || !hasNote(got.Corrections, "model_dropped") {
		t.Errorf("llm = %+v, corrections %+v", got.Input.LLM, got.Corrections)
	}
	if !strings.Contains(got.Corrections[0].Text, "unknown") {
		t.Errorf("corrections in the caller's language: %q", got.Corrections[0].Text)
	}
}

func TestDescribeRefusals(t *testing.T) {
	cases := map[string]string{
		"no json":       "Ich kann dabei leider nicht helfen.",
		"broken json":   `{"llm": {"tool": "code", `,
		"array":         `[1, 2, 3]`,
		"nothing valid": `{"llm": {"tool": "code", "persons": -3}, "cloud": {"flavors": [{"flavor": "nope", "count": 1}]}, "assumptions": []}`,
		"empty object":  `{}`,
	}
	for name, answer := range cases {
		d := NewDescriber(DefaultConfig(), &fakeLLM{answer: answer})
		if _, err := d.Describe(context.Background(), "x", describeCtx(), msg.DE); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: %v, want ErrUnreadable", name, err)
		}
	}
	d := NewDescriber(DefaultConfig(), &fakeLLM{err: errors.New("down")})
	if _, err := d.Describe(context.Background(), "x", describeCtx(), msg.DE); !errors.Is(err, ErrModelUnavailable) || errors.Is(err, ErrUnreadable) {
		t.Errorf("a failing model is its own error: %v", err)
	}
	if _, err := d.Describe(context.Background(), "   ", describeCtx(), msg.DE); err == nil {
		t.Error("an empty description is refused")
	}
	if _, err := d.Describe(context.Background(), strings.Repeat("a", MaxDescriptionLength+1), describeCtx(), msg.DE); err == nil {
		t.Error("a description too long is refused before the model is asked")
	}
}

func TestOpenAIClient(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["model"] == "broken" {
			http.Error(w, `{"error": "model not found"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "{\"assumptions\": []}"}}]}`))
	}))
	defer srv.Close()

	if (ClientConfig{URL: srv.URL}).Enabled() || (ClientConfig{APIKey: "k"}).Enabled() {
		t.Error("URL and key are both needed")
	}
	c := NewOpenAIClient(ClientConfig{URL: srv.URL + "/v1/", APIKey: "test-key"})
	out, err := c.Complete(context.Background(), "sys", "user", map[string]any{"type": "object"})
	if err != nil || out != `{"assumptions": []}` {
		t.Fatalf("answer %q, %v", out, err)
	}
	if auth != "Bearer test-key" || got["model"] != "chat-large" || got["temperature"].(float64) != 0 {
		t.Errorf("request: auth %q, body %v", auth, got)
	}
	rf := got["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Errorf("response_format = %v", rf)
	}
	c = NewOpenAIClient(ClientConfig{URL: srv.URL + "/v1", APIKey: "test-key", Model: "broken"})
	if _, err := c.Complete(context.Background(), "s", "u", nil); err == nil || !strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "test-key") {
		t.Errorf("an error status is an error without the key: %v", err)
	}
}
