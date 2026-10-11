package nodestore

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// The migration moves a fixed list of keys; a field added to Binding later is not stored at the top in any old node, so it does not belong on the list. But every key on the list must be one Binding reads, or the moved value would be lost on the next read.
func TestBoundNodeFields_AreTheKeysOfABinding(t *testing.T) {
	var keys []string
	bt := reflect.TypeFor[model.Binding]()
	for i := range bt.NumField() {
		keys = append(keys, strings.Split(bt.Field(i).Tag.Get("json"), ",")[0])
	}
	for _, f := range boundNodeFields {
		// The server counts are moved on into the facts by factBindingCounts.
		if !slices.Contains(keys, f) && !slices.Contains(factBindingFields, f) {
			t.Errorf("boundNodeFields moves %q, which model.Binding does not read", f)
		}
	}
}
