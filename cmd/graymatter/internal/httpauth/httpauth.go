// Package httpauth is the bearer-token gate in front of GrayMatter's two
// network listeners: the REST API (`graymatter server`) and the MCP
// StreamableHTTP transport (`graymatter mcp serve --http`).
//
// Both used to serve every route to anyone who could reach the port, and both
// bound to every interface by default, so a laptop on a café network handed
// out read, write and delete on the whole memory store. The daemon's RPC
// already solved this problem — a 256-bit token compared in constant time —
// and this package is the same idea over HTTP.
//
// The token lives in a file next to the store so it survives restarts: an
// agent config that hard-codes it keeps working, unlike the daemon's token
// which is regenerated per daemon.
package httpauth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

// TokenFile is the file in dataDir holding the shared HTTP bearer token.
const TokenFile = "graymatter.http-token"

// TokenEnv names the environment variable that overrides the token file.
// Handy for containers and CI, where writing into the data dir is awkward.
const TokenEnv = "GRAYMATTER_HTTP_TOKEN"

// TokenFilePath returns the absolute path to the HTTP token file in dataDir.
func TokenFilePath(dataDir string) string {
	return filepath.Join(dataDir, TokenFile)
}

// LoadOrCreateToken returns the HTTP bearer token for dataDir, generating and
// persisting one the first time. created reports whether this call minted it,
// so the caller can print it once instead of on every start.
//
// The environment variable wins when set, and is never written to disk.
//
// The file is written 0600 and then handed to the platform's real access
// control (rpc.SecureFileOwnerOnly): a protected owner-only DACL on Windows,
// where 0600 alone is not a promise. A failure to secure the file is an
// error — a token that cannot be protected must not be minted.
func LoadOrCreateToken(dataDir string) (token string, created bool, err error) {
	if env := strings.TrimSpace(os.Getenv(TokenEnv)); env != "" {
		return env, false, nil
	}

	path := TokenFilePath(dataDir)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, false, nil
		}
		// Empty file: fall through and mint a new token over it.
	case !errors.Is(err, os.ErrNotExist):
		return "", false, fmt.Errorf("httpauth: read token: %w", err)
	}

	tok, err := rpc.GenerateToken()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", false, fmt.Errorf("httpauth: create data dir: %w", err)
	}
	// Write to a temp file first so a reader never sees a half-written token.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("httpauth: write token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", false, fmt.Errorf("httpauth: install token: %w", err)
	}
	if err := rpc.SecureFileOwnerOnly(path); err != nil {
		_ = os.Remove(path)
		return "", false, fmt.Errorf("httpauth: secure token file: %w", err)
	}
	return tok, true, nil
}

// Middleware wraps next with a bearer-token check. An empty token is a
// programming error rather than "auth off": it rejects everything, so a
// mistake fails closed.
//
// The comparison is constant time. The 401 body says nothing beyond
// "unauthorized" — a caller who does not have the token has no business
// learning whether it was missing, malformed or merely wrong.
func Middleware(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(BearerToken(r))
		if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="graymatter"`)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BearerToken extracts the credential from an Authorization header. The scheme
// match is case-insensitive because RFC 7235 says it is.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// IsLoopback reports whether a listen address is reachable only from this
// machine. A host-less address like ":8080" is not: Go binds it to every
// interface, which is how the REST server ended up on the LAN by default.
//
// An unresolvable host counts as non-loopback, so an unknown address gets the
// warning rather than silent trust.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port at all; treat the whole string as the host.
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false // ":8080" — all interfaces
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ExposureWarning returns the text to print when a listener is about to accept
// connections from outside this machine, or "" when the bind is loopback-only.
// Callers print it to stderr; nothing here decides policy.
func ExposureWarning(addr string, authEnabled bool) string {
	if IsLoopback(addr) {
		return ""
	}
	var b strings.Builder
	b.WriteString("WARNING: listening on " + addr + ", which is reachable from the network.\n")
	b.WriteString("         GrayMatter memory holds whatever your agents were told, so treat\n")
	b.WriteString("         this port as sensitive. Bind 127.0.0.1 unless you meant this.\n")
	if !authEnabled {
		b.WriteString("         Authentication is DISABLED: anyone who can reach this port can\n")
		b.WriteString("         read, write and delete every agent's memory.\n")
	}
	return b.String()
}
