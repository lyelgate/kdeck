package quick

import (
	"strings"
	"testing"
)

func TestNeat(t *testing.T) {
	in := `{"apiVersion":"v1","kind":"Service","metadata":{"name":"web","namespace":"shop","uid":"u","resourceVersion":"9",
"creationTimestamp":"2026-01-01T00:00:00Z","managedFields":[{}],"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{}"},
"labels":{"app":"web"}},"spec":{"clusterIP":"10.0.0.1","clusterIPs":["10.0.0.1"],"ports":[{"port":80}],"selector":{"app":"web"}},"status":{"loadBalancer":{}}}`
	out, err := Neat([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, gone := range []string{"uid", "resourceVersion", "managedFields", "creationTimestamp", "last-applied", "clusterIP", "status", "annotations"} {
		if strings.Contains(s, gone) {
			t.Errorf("%q should be removed:\n%s", gone, s)
		}
	}
	for _, kept := range []string{"name: web", "namespace: shop", "app: web", "port: 80"} {
		if !strings.Contains(s, kept) {
			t.Errorf("%q should be kept:\n%s", kept, s)
		}
	}
}
