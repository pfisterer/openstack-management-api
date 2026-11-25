package webserver

import "testing"

// The deployed tag (APP_VERSION from the chart) wins over the version baked into the binary: a stable release retags a pre-release image without rebuilding it.
func TestRunningVersion(t *testing.T) {
	cases := []struct{ env, embedded, want string }{
		{"0.8.91", "0.8.90-test.12\n", "0.8.91"},
		{" 0.8.91 \n", "0.8.90-test.12\n", "0.8.91"},
		{"", "0.8.90-test.12\n", "0.8.90-test.12"},
		{"   ", "0.8.90\n", "0.8.90"},
	}
	for _, c := range cases {
		if got := runningVersion(c.env, c.embedded); got != c.want {
			t.Errorf("runningVersion(%q, %q) = %q, want %q", c.env, c.embedded, got, c.want)
		}
	}
}
