package match

import "testing"

func TestSmartCase(t *testing.T) {
	for _, tc := range []struct {
		pattern, s string
		want       bool
	}{
		{"api", "payments-API-7d9f", true},  // lower-case pattern ignores case
		{"API", "payments-api-7d9f", false}, // upper-case pattern is exact
		{"^web-[0-9]+$", "web-12", true},
		{"^web-[0-9]+$", "web-12-canary", false},
		{"api|worker", "billing-worker-x", true},
		{`\S+-db`, "orders-DB", true}, // \S is not an upper-case letter
		{"", "anything", true},
	} {
		m, err := Compile(tc.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Match(tc.s); got != tc.want {
			t.Errorf("%q on %q: got %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
	if _, err := Compile("("); err == nil {
		t.Error("expected an error for an invalid regex")
	}
	var nilM *Matcher
	if !nilM.Match("x") || !nilM.Empty() {
		t.Error("nil matcher should match everything")
	}
}
