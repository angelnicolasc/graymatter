module github.com/angelnicolasc/graymatter/cmd/graymatter

go 1.25.5

// Recommend the patched toolchain for standalone builds of this module.
// Go 1.26.11 includes the net/http fix for GO-2026-6859.
// The go directive sets the minimum supported version. Builds inside go.work
// and versioned go install commands select their toolchain independently.
toolchain go1.26.11

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
	charm.land/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 v2.0.9
	charm.land/lipgloss/v2 v2.0.6
	github.com/BurntSushi/toml v1.4.0
	github.com/angelnicolasc/graymatter v0.21.0
	github.com/anthropics/anthropic-sdk-go v1.33.0
	github.com/atotto/clipboard v0.1.4
	github.com/charmbracelet/x/ansi v0.11.8
	github.com/mark3labs/mcp-go v0.58.0
	github.com/oklog/ulid/v2 v2.1.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	github.com/spf13/cobra v1.8.1
	go.etcd.io/bbolt v1.3.11
	golang.org/x/sys v0.47.0
	golang.org/x/text v0.39.0
)

require (
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-runewidth v0.0.27 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/philippgille/chromem-go v0.7.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/sahilm/fuzzy v0.1.3 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/sync v0.22.0 // indirect
)
