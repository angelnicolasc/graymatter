# Terminal workbench

The workbench opens on **Memory**. It supports ordinary terminal fonts, keyboard
navigation, Unicode cell widths, optional mouse navigation, and dark, light and
terminal themes. It uses the existing store and daemon; there is no second
memory database.

The dark theme uses charcoal surfaces, teal navigation and restrained semantic
colors. Memory text comes before record metadata; Usage keeps quantities in
aligned rows and source details behind `v`. Header and list spacing adapt to
terminal height so larger windows have breathing room without consuming the
same proportion of a small terminal.

```sh
graymatter tui
graymatter tui --theme light
graymatter tui --mouse
graymatter tui --read-only
graymatter tui --read-only --no-daemon --dir /path/to/existing/store
graymatter demo
```

The demo is offline and uses an isolated sample store. Its quota, cost and
context observations are synthetic and labeled accordingly. `demo --no-tui`
seeds the same data without opening the interactive view.

## Navigation

| Control | Action |
|---|---|
| `1`–`6` | Memory, Recall, Activity, Graph, Usage, Status |
| `/`, `:`, `Ctrl+K` | Search the command palette |
| `n` | Select a namespace; writable sessions can select a new name |
| `f`, `Ctrl+F` | Filter the current list; enter a query in Recall |
| `Tab`, `Shift+Tab` | Move focus between panes |
| Arrows, `j`/`k`, Page Up/Down | Navigate the focused list or inspector |
| `Enter` | Open the selection or submit an input |
| `Esc` | Cancel an input/dialog or return to the list |
| `r` | Refresh the current resource |
| `Ctrl+L` | Redraw the terminal after external output or a resize artifact |
| `y` | Copy selected content to the system clipboard; request OSC52 if that fails |
| `?` | Show keyboard help |
| `q`, `Ctrl+C` | Quit; inputs capture `q`, and dialogs use `Esc` to return; `Ctrl+C` always quits |

Mouse navigation is off by default to preserve native terminal selection.
Enable it with `--mouse` or the `/mouse` command. `NO_COLOR` disables color;
focus and state also have text markers. Below 100 columns the list and inspector
share the screen; `Tab` opens the inspector. `Enter` also opens it in Memory,
Activity and Graph; in Recall, `Enter` opens the query input. At 120 columns the
Memory namespace sidebar becomes visible. Layout tests include 80×24, 100×30, 120×36
and 160×48 cells. The minimum useful emergency layout is 30×10.

OSC52 clipboard requests depend on terminal support and cannot be acknowledged
by the workbench. If copy is unavailable, the inspector retains the text for
native terminal selection.

Namespace, lifecycle filter, text filter, theme and the selected fact are saved
per store under the operating system's user configuration directory. Selection
restoration is best effort within the first loaded page. An explicit `--theme`
takes precedence over the saved theme. Demo sessions do not read or write these
preferences.

## Memory and curation

Use `[` / `]` to select active content, retired facts, aliases or all records.
The first-open filter includes active content only; later sessions restore the
saved lifecycle filter. Literal text filtering runs on
the store side. Pages transfer at most 100 lightweight facts; `Ctrl+N` loads the
next page. Rows preserve metadata and omit embeddings. Counts are exact for
that read, so obtaining a page still scans the matching namespace. Concurrent
pages are live reads rather than a transaction spanning the whole UI session.

`a` adds, `e` revises, `d` retires, and `p` toggles the selected fact's pin.
Editors submit with `Ctrl+S`; Enter inserts a newline. Retirement requires a
confirmation and removes the row from recall. The retired row remains
inspectable until ordinary pruning or explicit deletion. Revision targets the
selected ID and commits the replacement, retirement, and version lineage
together with conservative confidence. A pin is not inherited by the
replacement. Stale content, confidence
or pin state causes a conflict instead of updating a different fact. Inspect the
fresh selection before retrying. A lost write response is reported as an
unknown outcome and is not automatically replayed. Further mutations remain
blocked until the affected namespace or run list has refreshed successfully.

The inspector distinguishes retention weight, confidence, lifecycle, timestamps
and access counts. Valid confidence labels are marked writer-declared; an absent
legacy label is not declared and has effective confidence `inferred`. Unknown
historical labels retain their original text and have effective confidence
`unverified`. Retention weight measures how long a fact is kept.

Memory and Recall show namespace-scoped version lineage where recorded.
Press `v` to inspect a selected version. Use `Tab` or `Shift+Tab` to choose a
versioned source, `Enter` to follow it, `Backspace` to return to the previous
version, and `Esc` to close. When current content has diverged, the preview
shows both hashes and `v` opens the historical version. An ambiguous legacy
pointer can be followed when its exact source snapshot is available.
Inspection is read-only. Retained evidence stays outside recall.

## Recall, Activity and Graph

Recall runs after submitting a query, or pressing `r` to repeat the current
query. With no query yet, `r` prompts you to enter one. Its ranking receipts
expose signal ranks and the base RRF score. When ranking-policy metadata is
present, they also show the final selection score, confidence factor, policy and
confidence weight; the base score is not presented as the final score.
Inspection does not increment accesses, learn aliases,
fire recall hooks or append graph enrichment. The configured
embedding provider may still receive the query; inspection does not invoke a
generative model. An older daemon that lacks the inspection capability returns
an update/restart error rather than substituting ordinary recall.

Activity lists store-wide runs started by `graymatter run`, not every external client's
conversation. `c` switches to checkpoints for the selected namespace. `x`
requests a confirmed stop of a running harness session; `k` only navigates.

Graph is store-wide. `o` browses connected entities and `s` resolves a recorded
supporting fact. Missing source IDs remain missing; the view does not invent
provenance. Inspector scrolling is retained until the selection changes.

## Usage and Status

Usage has `a` Auto, `l` Limits and `s` Spend modes. `c` expands session context,
`v` toggles source details, and `g` switches the footer summary between limits
and spending. Context is independent of the billing mode. `r` explicitly
refreshes enabled account connections. Startup reads cached observations only.
The TUI bounds its display to 100 quota windows, 100 recent effective cost
observations, 20 recent session contexts and five recent request events, with
an explicit notice when more records exist. CLI JSON exposes the full bounded
snapshot. Cost precedence and event sorting run in the asynchronous loader,
not while navigating the viewport.
See [Usage setup and data contracts](usage.md) for supported adapters, imports,
credentials, estimates and scope limitations.

Status labels stored records, fact accesses, estimated payload bytes and
retention weight according to what they measure. Provider configuration is
distinct from reachability; reading the database does not prove a model endpoint
is healthy. Each resource retains its last-success timestamp and error. Store
failures do not become a healthy all-zero dashboard.

## Read-only operation

`--read-only` blocks memory, graph, checkpoint, session and accounting mutations
at the client boundary without changing permissions for other daemon clients.
It requires an existing regular `gray.db` and never starts a daemon. Use the
running daemon for concurrent inspection, or choose `--no-daemon` explicitly
for a direct read-only handle. A direct bbolt reader cannot bypass a writer's
exclusive file lock.

Recall preview also suppresses index repair and other hidden store writes.
Explicit Usage refresh can update the separate user Usage cache; UI preferences
are separate from the memory store. Read-only mode is a memory/session policy,
not a prohibition on all filesystem or network activity.

## Validation and practical limits

The state tests cover input focus, exact selection, confirmation, stale async
results, read-only guards, reconnect errors, Unicode cell bounds and terminal
control sanitization. Integration tests exercise direct and daemon-backed
inspection, curation and Usage reporting against deterministic fixtures.

Provider credentials are opt-in. Tests against synthetic reporting servers do
not establish that a particular user's subscription or administrative account
is eligible. The terminal emulator, clipboard integration and remote shell can
also affect input behavior; see the [validation record](validation/tui-workbench.md)
for the environments actually checked.
