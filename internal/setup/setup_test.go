package setup

import "testing"

func TestSkew(t *testing.T) {
	for _, tc := range []struct {
		client, server string
		diff           int
		ok             bool
	}{
		{"v0.37.2", "v1.37.0", 0, true},
		{"v0.37.2", "v1.38.1", 1, true},
		{"v0.37.2", "v1.36.4-eks-abc", -1, true},
		{"v0.37.2", "v1.33.2", -4, false},
		{"v0.37.2", "v1.39.0", 2, false},
		{"unknown", "v1.30.0", 0, true},
	} {
		d, ok := Skew(tc.client, tc.server)
		if d != tc.diff || ok != tc.ok {
			t.Errorf("%s vs %s: got %d %v, want %d %v", tc.client, tc.server, d, ok, tc.diff, tc.ok)
		}
	}
}
