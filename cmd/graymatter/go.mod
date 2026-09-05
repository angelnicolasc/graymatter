module github.com/angelnicolasc/graymatter/cmd/graymatter

go 1.25.5

// The binaries built from this module ship to users, and `go install ...@latest`
// bypasses CI entirely — whatever toolchain the user happens to have is what
// links the stdlib in. govulncheck found 14 reachable stdlib advisories against
// 1.26.1; all are fixed by 1.26.6. Declaring the toolchain here means a user on
// an older Go still gets a patched binary.
//
// This is deliberately not in the root go.mod: that module is consumed as a
// library, and forcing a toolchain download on library consumers is not ours
// to decide. Their own toolchain links their own binary.
//
// It also has no effect on this repo's own builds, which run in the go.work
// workspace, where go.work's directives govern toolchain selection.
toolchain go1.26.7

// The library is developed one directory up in the same checkout. The
// go.work workspace at the repo root maps that dependency to the checkout for
// every in-repo build, so workspace builds always compile against the working
// tree.
//
// A replace directive CANNOT stand in for the workspace here: go refuses
// `go install module@version` when the providing module carries replace
// directives (issue #75 — verified on v0.14.0 through v0.15.0). This module
// is what users install, so it must stay replace-free; the cost is that a
// non-workspace build (GOWORK=off) resolves the library at the published
// version required below — which is exactly the contract `go install
// @version` exercises, and CI runs that build as a gate.
//
// Bump the require below with every release, together with the version bump
// in CHANGELOG.md: an install from a fresh tag resolves the library at this
// version, and a stale pin silently ships an old core (the release workflow's
// install-smoke step fails on that).

require (
	github.com/BurntSushi/toml v1.4.0
	github.com/angelnicolasc/graymatter v0.19.0
	github.com/anthropics/anthropic-sdk-go v1.33.0
	github.com/charmbracelet/bubbles v0.20.0
	github.com/charmbracelet/bubbletea v1.3.4
	github.com/charmbracelet/lipgloss v1.1.0
	github.com/mark3labs/mcp-go v0.58.0
	github.com/muesli/termenv v0.16.0
	github.com/oklog/ulid/v2 v2.1.0
	github.com/spf13/cobra v1.8.1
	go.etcd.io/bbolt v1.3.11
	golang.org/x/sys v0.34.0
)

require (
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/charmbracelet/colorprofile v0.2.3-0.20250311203215-f60798e515dc // indirect
	github.com/charmbracelet/x/ansi v0.8.0 // indirect
	github.com/charmbracelet/x/cellbuf v0.0.13-0.20250311204145-2c3ea96c31dd // indirect
	github.com/charmbracelet/x/term v0.2.1 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.16 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/philippgille/chromem-go v0.7.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/sahilm/fuzzy v0.1.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/sync v0.16.0 // indirect
	golang.org/x/text v0.27.0 // indirect
)
