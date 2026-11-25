package assistant

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/msg"
)

// The evaluation set (testdata/eval.json): German questions with the tools a good answer starts with, tools that must never be asked for, and what stage 2 should propose. TestEvalSetIsWellFormed always runs; TestEvalAgainstModel only with a model (ASSISTANT_EVAL_URL, ASSISTANT_EVAL_API_KEY, optionally ASSISTANT_EVAL_MODEL), against fixtures instead of a database, and reports per question rather than failing on taste: it fails only on a write tool asked for or a question that errors.

type evalSet struct {
	Fixtures map[string]json.RawMessage `json:"fixtures"`
	Cases    []evalCase                 `json:"cases"`
}

type evalCase struct {
	Question       string   `json:"question"`
	First          []string `json:"first"`
	Any            []string `json:"any"`
	Never          []string `json:"never"`
	AnswerContains string   `json:"answer_contains"`
	// OffTopic: the question is outside the portal and must get the fixed refusal (Answer.OffTopic); false: it must not.
	OffTopic bool `json:"off_topic"`
	Stage2   *struct {
		Action string `json:"action"`
		Dialog string `json:"dialog"`
	} `json:"stage2"`
}

// writeTools are the MCP tools that change something; the stage-2 actions name them.
var writeTools = []string{"create_project", "request_project_change", "add_project_allocation", "approve_request", "reject_request", "rename_project", "set_project_admins", "create_budget", "move_to_budget", "transfer_ownership", "adopt_imported_project", "release_project", "end_allocation", "delete_project", "delete_budget"}

var stage2Dialogs = []string{"project_new", "allocation_change", "project_extend", "allocation_add", "members", "decide", "rename"}

func loadEval(t *testing.T) evalSet {
	t.Helper()
	raw, err := os.ReadFile("testdata/eval.json")
	if err != nil {
		t.Fatal(err)
	}
	var s evalSet
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEvalSetIsWellFormed(t *testing.T) {
	s := loadEval(t)
	if len(s.Cases) < 10 {
		t.Errorf("only %d cases", len(s.Cases))
	}
	for _, name := range ReadTools {
		if _, ok := s.Fixtures[name]; !ok {
			t.Errorf("no fixture for read tool %s", name)
		}
	}
	for _, c := range s.Cases {
		for _, n := range append(slices.Clone(c.First), c.Any...) {
			if !slices.Contains(ReadTools, n) {
				t.Errorf("%q expects %s, not a read tool", c.Question, n)
			}
		}
		for _, n := range c.Never {
			if !slices.Contains(writeTools, n) {
				t.Errorf("%q forbids %s, not a write tool", c.Question, n)
			}
		}
		if c.Stage2 != nil && (!slices.Contains(writeTools, c.Stage2.Action) || !slices.Contains(stage2Dialogs, c.Stage2.Dialog)) {
			t.Errorf("%q: stage 2 %+v unknown", c.Question, *c.Stage2)
		}
	}
}

// fixtureBox offers the read tools and answers each from the fixtures, whatever the arguments.
type fixtureBox struct {
	fixtures map[string]json.RawMessage
	called   []string
}

func (b *fixtureBox) Tools(context.Context) ([]ToolDef, error) {
	var out []ToolDef
	for _, n := range ReadTools {
		out = append(out, ToolDef{Name: n, Description: "see the MCP tool " + n, Parameters: map[string]any{"type": "object", "additionalProperties": true}})
	}
	return out, nil
}

func (b *fixtureBox) Call(_ context.Context, name string, _ json.RawMessage) (string, bool, error) {
	b.called = append(b.called, name)
	return string(b.fixtures[name]), false, nil
}

func (b *fixtureBox) Close() error { return nil }

func TestEvalAgainstModel(t *testing.T) {
	url, key := os.Getenv("ASSISTANT_EVAL_URL"), os.Getenv("ASSISTANT_EVAL_API_KEY")
	if url == "" || key == "" {
		t.Skip("ASSISTANT_EVAL_URL and ASSISTANT_EVAL_API_KEY unset")
	}
	model := os.Getenv("ASSISTANT_EVAL_MODEL")
	s := loadEval(t)
	cc := ClientConfig{URL: url, APIKey: key, Model: model, TimeoutSeconds: 120}
	agent := &Agent{Model: NewOpenAIClient(cc, 600), Classifier: NewOpenAIClient(cc, 64), Now: func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) }}
	good := 0
	for _, c := range s.Cases {
		box := &fixtureBox{fixtures: s.Fixtures}
		ans, err := agent.Ask(context.Background(), Question{Question: c.Question}, box, msg.DE)
		if err != nil {
			t.Errorf("%q: %v", c.Question, err)
			continue
		}
		var asked []string
		for _, r := range ans.ToolCalls {
			asked = append(asked, r.Name)
		}
		ok := true
		if len(c.First) > 0 && (len(asked) == 0 || !slices.Contains(c.First, asked[0])) {
			ok = false
		}
		for _, n := range c.Any {
			if !slices.Contains(asked, n) {
				ok = false
			}
		}
		for _, n := range c.Never {
			if slices.Contains(asked, n) {
				t.Errorf("%q: asked for the write tool %s", c.Question, n)
				ok = false
			}
		}
		if c.AnswerContains != "" && !strings.Contains(ans.Answer, c.AnswerContains) {
			ok = false
		}
		if ans.OffTopic != c.OffTopic {
			ok = false
		}
		if ok {
			good++
		}
		t.Logf("%-5v %q\n  tools %v, steps %d, tokens %d+%d, stopped %q\n  %s", ok, c.Question, asked, ans.Usage.Steps, ans.Usage.PromptTokens, ans.Usage.CompletionTokens, ans.Stopped, ans.Answer)
	}
	t.Logf("%d of %d as expected", good, len(s.Cases))
}
