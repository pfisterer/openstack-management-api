package common

import (
	"strings"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/msg"
)

// A cap's default is what the UI pre-fills when the cap is switched on, so it must be a value the cap allows.
func TestValidateCapDefs_Default(t *testing.T) {
	base := func(def *int) Cap {
		return Cap{ID: "jobs", Name: msg.Raw("Jobs"), Provider: "p", Min: 1, Max: 10, Default: def}
	}
	for _, v := range []int{1, 5, 10} {
		if err := ValidateCapDefs([]Cap{base(new(v))}); err != nil {
			t.Errorf("default %d within 1..10 should pass: %v", v, err)
		}
	}
	if err := ValidateCapDefs([]Cap{base(nil)}); err != nil {
		t.Errorf("a cap without a default should pass: %v", err)
	}
	for _, v := range []int{0, 11, -1} {
		if err := ValidateCapDefs([]Cap{base(new(v))}); err == nil || !strings.Contains(err.Error(), "default") {
			t.Errorf("default %d outside 1..10 should be refused naming the default, got %v", v, err)
		}
	}

	unbounded := base(new(0))
	unbounded.Unbounded = true
	if err := ValidateCapDefs([]Cap{unbounded}); err != nil {
		t.Errorf("0 is a valid default of an unbounded cap: %v", err)
	}

	choice := Cap{ID: "prio", Name: msg.Raw("Prio"), Provider: "p", Choices: []string{"low", "high"}, Default: new(1)}
	if err := ValidateCapDefs([]Cap{choice}); err != nil {
		t.Errorf("a choice's index should pass as default: %v", err)
	}
	choice.Default = new(2)
	if err := ValidateCapDefs([]Cap{choice}); err == nil {
		t.Error("a default past the choices should be refused")
	}
}

// The refusals of Check and the labels in messages are German, like the tree's validation messages; a choice keeps its value, which is what the provider is told.
func TestCheckAndLabel_German(t *testing.T) {
	hours := Cap{ID: "h", Name: msg.Raw("GPU: Obergrenze"), Provider: "p", Unit: "h", Min: 1, Max: 720, Unbounded: true}
	runs := Cap{ID: "r", Name: msg.Raw("Laufzeit"), Provider: "p", Unit: "h", Min: 1, Max: 24}
	prio := Cap{ID: "c", Name: msg.Raw("Priorität"), Provider: "p", Choices: []string{"preemptible", "guaranteed"}}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{hours.Check(800), "GPU: Obergrenze: Erlaubt sind 1 bis 720 h oder 0 für keine Obergrenze."},
		{runs.Check(0), "Laufzeit: Erlaubt sind 1 bis 24 h."},
		{prio.Check(2), "Priorität: 2 ist keiner der Werte 0 bis 1 (preemptible, guaranteed)."},
	} {
		if tc.err == nil || tc.err.Error() != tc.want {
			t.Errorf("got %v, want %q", tc.err, tc.want)
		}
	}
	if got := hours.Label(0); got.In(msg.DE) != "ohne Obergrenze" || got.In(msg.EN) != "no upper bound" {
		t.Errorf("Label(0) under Unbounded = %q", got)
	}
	if got := prio.Label(1); got.String() != "guaranteed" {
		t.Errorf("a choice's label = %q, want its value", got)
	}
}
