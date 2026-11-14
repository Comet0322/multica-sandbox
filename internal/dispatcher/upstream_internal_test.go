package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Only "runtime not found" from Multica's own 404 means the runtime is
// gone; the error text never carries the body.
func TestRuntimeNotFoundIsRecognisedFromTheBodyOfA404(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"the server's own message", http.StatusNotFound, `{"error":"runtime not found"}`, true},
		{"a different 404", http.StatusNotFound, `{"error":"not found"}`, false},
		{"an empty 404", http.StatusNotFound, ``, false},
		{"the marker with another status", http.StatusInternalServerError, `{"error":"runtime not found"}`, false},
		{"the marker with 403", http.StatusForbidden, `{"error":"runtime not found"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tc.body, tc.status)
			}))
			defer srv.Close()
			err := newUpstream(srv.URL, "tok", func() map[string]string { return nil }).call(context.Background(), http.MethodPost, "/api/daemon/heartbeat", map[string]any{}, nil)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := runtimeNotFound(err); got != tc.want {
				t.Fatalf("runtimeNotFound = %v, want %v (%v)", got, tc.want, err)
			}
			if strings.Contains(err.Error(), "runtime not found") && tc.body != "" {
				t.Fatalf("the error text must not carry the response body: %v", err)
			}
		})
	}
	if runtimeNotFound(context.Canceled) || runtimeNotFound(nil) {
		t.Fatal("only an upstream error can say the runtime is gone")
	}
}
