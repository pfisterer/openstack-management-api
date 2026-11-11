package common

import (
	"strings"
	"testing"
)

// A cap's default is what the UI pre-fills when the cap is switched on, so it must be a value the cap allows.
func TestValidateCapDefs_Default(t *testing.T) {
	base := func(def *int) Cap {
		return Cap{ID: "jobs", Name: "Jobs", Provider: "p", Min: 1, Max: 10, Default: def}
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

	choice := Cap{ID: "prio", Name: "Prio", Provider: "p", Choices: []string{"low", "high"}, Default: new(1)}
	if err := ValidateCapDefs([]Cap{choice}); err != nil {
		t.Errorf("a choice's index should pass as default: %v", err)
	}
	choice.Default = new(2)
	if err := ValidateCapDefs([]Cap{choice}); err == nil {
		t.Error("a default past the choices should be refused")
	}
}
