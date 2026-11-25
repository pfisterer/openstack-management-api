package osclient

import "testing"

func TestGPUsFromExtraSpecs(t *testing.T) {
	cases := []struct {
		specs map[string]string
		want  int
	}{
		{nil, 0},
		{map[string]string{"hw:cpu_policy": "dedicated"}, 0},
		{map[string]string{"resources:VGPU": "1"}, 1},
		{map[string]string{"pci_passthrough:alias": "a100:2"}, 2},
		{map[string]string{"pci_passthrough:alias": "a100:2, l4:1"}, 3},
		{map[string]string{"pci_passthrough:alias": "broken"}, 0},
		{map[string]string{"resources:VGPU": "x", "pci_passthrough:alias": "l4:1"}, 1},
	}
	for _, c := range cases {
		if got := GPUsFromExtraSpecs(c.specs); got != c.want {
			t.Errorf("GPUsFromExtraSpecs(%v) = %d, want %d", c.specs, got, c.want)
		}
	}
}
