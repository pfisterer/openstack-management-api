package tree

import (
	"reflect"
	"slices"
	"testing"
)

// Every field of UpdateNodeRequest has a class, so a new field cannot slip past the check of who may send it; and sending a field alone is seen as that class.
func TestUpdateNodeRequest_EveryFieldClassified(t *testing.T) {
	typ := reflect.TypeFor[UpdateNodeRequest]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		class, ok := updateFieldClass[f.Name]
		if !ok {
			t.Errorf("UpdateNodeRequest.%s has no class in updateFieldClass", f.Name)
			continue
		}
		var r UpdateNodeRequest
		v := reflect.ValueOf(&r).Elem().Field(i)
		switch v.Kind() {
		case reflect.Pointer:
			v.Set(reflect.New(f.Type.Elem()))
		case reflect.Map:
			v.Set(reflect.MakeMap(f.Type))
		default:
			v.SetBool(true)
		}
		if got := r.sentClasses(); len(got) != 1 || !got[class] {
			t.Errorf("UpdateNodeRequest.%s sent alone is seen as %v, want only %v", f.Name, got, class)
		}
		if r.leafDirectEdit() != (class == updateLabel) || r.policyEdit() != (class != updateCapacity) || r.capacityEdit() != (class == updateCapacity) {
			t.Errorf("UpdateNodeRequest.%s: leafDirectEdit %v, policyEdit %v, capacityEdit %v for class %v", f.Name, r.leafDirectEdit(), r.policyEdit(), r.capacityEdit(), class)
		}
	}
	if len(updateFieldClass) != typ.NumField() {
		t.Errorf("updateFieldClass has %d entries for %d fields: one names no field", len(updateFieldClass), typ.NumField())
	}
	var empty UpdateNodeRequest
	if empty.leafDirectEdit() || empty.policyEdit() || empty.capacityEdit() {
		t.Error("an empty request edits nothing")
	}
	if both := (UpdateNodeRequest{Name: new("x"), InheritsLimit: new(true)}); both.leafDirectEdit() {
		t.Error("a rename together with a capacity field is no direct edit of a project")
	}
}

// Every field of CreateNodeRequest is either common to both kinds, a project's, a budget setting a project refuses, or a budget field a project ignores; and each budget setting is refused by its JSON name.
func TestCreateNodeRequest_EveryFieldClassified(t *testing.T) {
	common := []string{"ParentID", "Kind", "Name", "Reason", "Limit", "TerminationDate", "AdminScope"}
	project := []string{"AuthorizedUsers"}
	typ := reflect.TypeFor[CreateNodeRequest]()
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		n := 0
		for _, list := range [][]string{common, project, createBudgetSettings, createIgnoredOnProject} {
			if slices.Contains(list, name) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("CreateNodeRequest.%s is in %d classes, want exactly 1", name, n)
		}
	}
	for _, name := range createBudgetSettings {
		var r CreateNodeRequest
		f, _ := typ.FieldByName(name)
		v := reflect.ValueOf(&r).Elem().FieldByName(name)
		switch v.Kind() {
		case reflect.Pointer:
			v.Set(reflect.New(f.Type.Elem()))
		case reflect.Map:
			v.Set(reflect.MakeMap(f.Type))
		default:
			v.SetBool(true)
		}
		if got, want := r.budgetSetting(), f.Tag.Get("json"); got != want {
			t.Errorf("CreateNodeRequest.%s: budgetSetting() = %q, want %q", name, got, want)
		}
	}
	if (CreateNodeRequest{}).budgetSetting() != "" {
		t.Error("an empty request sends no budget setting")
	}
}
