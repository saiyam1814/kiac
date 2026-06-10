package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/saiyam1814/kiac/pkg/cluster"
)

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"dev": true, "my-cluster-2": true,
		"": false, "Dev": false, "a b": false, "a/b": false, "a;rm": false, "a..b": false,
	} {
		if got := cluster.ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

// The UI serves cluster-admin kubeconfigs, so cross-origin pages and
// DNS-rebinding hosts must be rejected outright.
func TestLoopbackOnly(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := loopbackOnly("127.0.0.1:5180", inner)

	cases := []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:5180", "", 200},                       // curl / same-origin GET
		{"localhost:5180", "", 200},                       // browser via localhost
		{"127.0.0.1:5180", "http://127.0.0.1:5180", 200},  // same-origin fetch
		{"127.0.0.1:5180", "http://localhost:5180", 200},  // localhost-origin fetch
		{"evil.example:5180", "", 403},                    // DNS rebinding
		{"127.0.0.1:5180", "https://evil.example", 403},   // cross-origin fetch
		{"127.0.0.1:5180", "http://127.0.0.1:9999", 403},  // wrong-port origin
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "http://placeholder/api/clusters", nil)
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("host=%q origin=%q: got %d, want %d", c.host, c.origin, rec.Code, c.want)
		}
	}
}
