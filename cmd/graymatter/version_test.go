package main

import "testing"

// Every binary anyone installed used to call itself "dev". -ldflags is what
// sets the real number, and only GoReleaser passes it: `go install` and a
// plain `go build` do not, so the officially recommended install produced a
// binary that could not name its own version. Go embeds the module version in
// every binary regardless, which is what pickVersion falls back to.
func TestPickVersion(t *testing.T) {
	tests := []struct {
		name     string
		injected string
		module   string
		want     string
	}{
		{
			name:     "release build: the injected tag wins over the module version",
			injected: "0.12.1",
			module:   "v0.0.0-20260824033304-044d4ba44f2a",
			want:     "0.12.1",
		},
		{
			name:     "go install at a tag: the module version is the answer",
			injected: devVersion,
			module:   "v0.12.1",
			want:     "v0.12.1",
		},
		{
			name:     "go install @latest: a pseudo-version still beats \"dev\"",
			injected: devVersion,
			module:   "v0.0.0-20260824033304-044d4ba44f2a",
			want:     "v0.0.0-20260824033304-044d4ba44f2a",
		},
		{
			name:     "no module info at all: nothing better than dev",
			injected: devVersion,
			module:   "",
			want:     devVersion,
		},
		{
			name:     "an unversioned local build reports dev, not \"(devel)\"",
			injected: devVersion,
			module:   "(devel)",
			want:     devVersion,
		},
		{
			name:     "empty ldflags value is not a version",
			injected: "",
			module:   "v0.12.1",
			want:     "v0.12.1",
		},
		{
			name:     "empty everywhere still yields a printable answer",
			injected: "",
			module:   "",
			want:     devVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickVersion(tt.injected, tt.module); got != tt.want {
				t.Errorf("pickVersion(%q, %q) = %q, want %q",
					tt.injected, tt.module, got, tt.want)
			}
		})
	}
}

// resolveVersion runs against this test binary's own build info. It must never
// return an empty string: it feeds `--version`, the TUI header and the MCP
// handshake, and an empty version there reads as a broken install.
func TestResolveVersionIsNeverEmpty(t *testing.T) {
	if got := resolveVersion(); got == "" {
		t.Error("resolveVersion() returned an empty string")
	}
}
