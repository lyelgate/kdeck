package alias

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseCombo(t *testing.T) {
	for _, tc := range []struct {
		word string
		want string // "" means not a valid combination
	}{
		{"kgpo", "get pods"},
		{"gpo", "get pods"},
		{"kgpoa", "get pods --all-namespaces"},
		{"kgpoall", "get pods --all-namespaces"},
		{"kgpooyaml", "get pods -o yaml"},
		{"kgpoowide", "get pods -o wide"},
		{"kgdepw", "get deployment --watch"},
		{"kgpvc", "get persistentvolumeclaim"},
		{"kgpv", "get persistentvolume"},
		{"ksysgpo", "get pods --namespace=kube-system"},
		{"kdpol", "describe pods -l"},
		{"kgposll", "get pods --show-labels -l"},
		{"krmdep", "delete deployment"},
		{"klop", "logs -f -p"},
		{"kex", "exec -i -t"},
		{"ka", "apply -f"},
		{"kgpooyamlw", ""}, // yaml + watch is not allowed
		{"kgpooyamlojson", ""},
		{"ksysgpoall", ""}, // kube-system + all namespaces conflict
		{"kgpoln", ""},     // two value modifiers
		{"kgpoo", ""},      // "o" alone is not a modifier
		{"get", ""},
		{"logs", ""},
		{"describe", ""},
		{"apply", ""},
		{"explain", ""},
	} {
		got, ok := ParseCombo(tc.word)
		if tc.want == "" {
			if ok {
				t.Errorf("%s: expected no expansion, got %q", tc.word, got)
			}
			continue
		}
		if !ok || strings.Join(got, " ") != tc.want {
			t.Errorf("%s: got %q (ok=%v), want %q", tc.word, strings.Join(got, " "), ok, tc.want)
		}
	}
}

func TestExpand(t *testing.T) {
	isCmd := func(w string) bool { return w == "get" || w == "silent" || w == "ctx" }
	user := map[string]string{"kgr": "get pods --field-selector=status.phase=Running"}
	for _, tc := range []struct {
		in, want, used string
	}{
		{"kgpo", "get pods", "kgpo"},
		{"-n prod kgpo", "-n prod get pods", "kgpo"},
		{"--context=x kgpoa", "--context=x get pods --all-namespaces", "kgpoa"},
		{"kdpol app=web", "describe pods -l app=web", "kdpol"},
		{"l web-0 -c app", "logs web-0 -c app", "l"},
		{"rr deploy/api", "rollout restart deploy/api", "rr"},
		{"kgr -A", "get pods --field-selector=status.phase=Running -A", "kgr"},
		{"get pods", "get pods", ""},
		{"silent", "silent", ""},
		{"ctx prod", "ctx prod", ""},
		{"", "", ""},
	} {
		got, used := Expand(strings.Fields(tc.in), user, isCmd)
		if strings.Join(got, " ") != tc.want || used != tc.used {
			t.Errorf("%q: got %q (used %q), want %q (used %q)", tc.in, strings.Join(got, " "), used, tc.want, tc.used)
		}
	}
}

func TestSplitWords(t *testing.T) {
	got := SplitWords(`get pods -l 'app in (a, b)' -o "custom-columns=NAME:.metadata.name"`)
	want := []string{"get", "pods", "-l", "app in (a, b)", "-o", "custom-columns=NAME:.metadata.name"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCuratedAreValid(t *testing.T) {
	for _, w := range Curated {
		if _, ok := ParseCombo(w); !ok {
			t.Errorf("curated alias %q does not parse", w)
		}
	}
	if n := len(All()); n < 500 {
		t.Errorf("expected hundreds of generated aliases, got %d", n)
	}
}
