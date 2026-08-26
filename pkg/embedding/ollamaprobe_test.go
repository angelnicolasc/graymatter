package embedding

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaReachableRequiresExactOK(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusOK, true},
		{http.StatusNotFound, false}, // captive portal / proxy answering for unknown paths
		{http.StatusUnauthorized, false},
		{http.StatusInternalServerError, false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		if got := ollamaReachable(srv.URL); got != tc.want {
			t.Errorf("status %d: reachable = %v, want %v", tc.status, got, tc.want)
		}
		srv.Close()
	}

	if ollamaReachable("") {
		t.Error("empty URL must be unreachable")
	}
	if ollamaReachable("http://127.0.0.1:1") {
		t.Error("connection refused must be unreachable")
	}
}
