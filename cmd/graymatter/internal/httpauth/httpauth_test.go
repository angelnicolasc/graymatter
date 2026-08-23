package httpauth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateToken_PersistsAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(TokenEnv, "")

	tok, created, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !created {
		t.Error("first call should report the token as freshly created")
	}
	// 256 bits, hex-encoded — the same shape as the daemon's RPC token.
	if len(tok) != 64 {
		t.Errorf("token length = %d, want 64 hex chars; got %q", len(tok), tok)
	}

	again, created, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if created {
		t.Error("second call should reuse the stored token, not mint a new one")
	}
	if again != tok {
		t.Errorf("token changed between calls: %q then %q", tok, again)
	}
}

func TestLoadOrCreateToken_EnvOverrideIsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(TokenEnv, "  from-the-environment  ")

	tok, created, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if created {
		t.Error("an environment token is not something we created")
	}
	if tok != "from-the-environment" {
		t.Errorf("token = %q, want the trimmed environment value", tok)
	}
	if _, err := os.Stat(TokenFilePath(dir)); !os.IsNotExist(err) {
		t.Errorf("environment token leaked to disk at %s", TokenFilePath(dir))
	}
}

func TestLoadOrCreateToken_ReplacesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(TokenEnv, "")
	if err := os.WriteFile(TokenFilePath(dir), []byte("   \n"), 0o600); err != nil {
		t.Fatalf("seed empty token file: %v", err)
	}

	tok, created, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !created || len(tok) != 64 {
		t.Errorf("empty token file should be replaced; created=%v token=%q", created, tok)
	}
}

func TestLoadOrCreateToken_FileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		// 0600 is a POSIX guarantee. On Windows the file inherits the parent
		// directory's ACL, same as the daemon's discovery file.
		t.Skip("file modes are not enforced on Windows")
	}
	dir := t.TempDir()
	t.Setenv(TokenEnv, "")
	if _, _, err := LoadOrCreateToken(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	info, err := os.Stat(TokenFilePath(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
}

func TestLoadOrCreateToken_LeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(TokenEnv, "")
	if _, _, err := LoadOrCreateToken(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
}

func TestMiddleware(t *testing.T) {
	const token = "correct-horse-battery-staple"

	tests := []struct {
		name       string
		configured string
		header     string
		wantStatus int
	}{
		{"valid token", token, "Bearer " + token, http.StatusOK},
		{"lowercase scheme", token, "bearer " + token, http.StatusOK},
		{"extra whitespace", token, "Bearer   " + token + "  ", http.StatusOK},
		{"no header", token, "", http.StatusUnauthorized},
		{"wrong token", token, "Bearer nope", http.StatusUnauthorized},
		{"empty credential", token, "Bearer ", http.StatusUnauthorized},
		{"wrong scheme", token, "Basic " + token, http.StatusUnauthorized},
		{"prefix of token", token, "Bearer " + token[:len(token)-1], http.StatusUnauthorized},
		// An unconfigured server must reject everything, including the empty
		// credential that would otherwise "match" an empty token.
		{"unconfigured rejects empty", "", "", http.StatusUnauthorized},
		{"unconfigured rejects bearer", "", "Bearer ", http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			h := Middleware(tc.configured, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/facts", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if want := tc.wantStatus == http.StatusOK; reached != want {
				t.Errorf("handler reached = %v, want %v", reached, want)
			}
			if tc.wantStatus == http.StatusUnauthorized {
				if got := rec.Header().Get("WWW-Authenticate"); got == "" {
					t.Error("401 without a WWW-Authenticate header")
				}
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"LocalHost:8080", true},
		{"[::1]:8080", true},
		{"127.9.9.9:8080", true},
		// The default that caused the finding: no host means every interface.
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{"192.168.1.10:8080", false},
		{"example.com:8080", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := IsLoopback(tc.addr); got != tc.want {
			t.Errorf("IsLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestExposureWarning(t *testing.T) {
	if got := ExposureWarning("127.0.0.1:8080", true); got != "" {
		t.Errorf("loopback bind should not warn, got %q", got)
	}
	if got := ExposureWarning(":8080", true); got == "" {
		t.Error("a wildcard bind should warn")
	}
	withAuth := ExposureWarning("0.0.0.0:8080", true)
	withoutAuth := ExposureWarning("0.0.0.0:8080", false)
	if len(withoutAuth) <= len(withAuth) {
		t.Error("dropping authentication should add to the warning, not shorten it")
	}
}
