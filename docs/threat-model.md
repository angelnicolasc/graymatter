# GrayMatter Threat Model

What GrayMatter defends, what it does not, and where the boundary sits. Read
this before exposing a GrayMatter store to anything beyond one person's laptop.

---

## The one-line version

**Stored memory is untrusted input.** Everything else follows from that.

A fact in the store is text that some earlier process decided to keep. It may
have come from the user, from an agent's own reasoning, from a web page an
agent read, or from anything that could reach a write surface. GrayMatter
records facts; it does not vouch for them.

---

## Assets

| Asset | Why it matters |
|---|---|
| Facts in `gray.db` | Whatever agents were told: preferences, client details, credentials mentioned in passing |
| Checkpoints | Message history, resumable session state |
| The agent's behaviour | Recalled facts land in the system prompt, so memory influences what the agent does next |
| The daemon's socket / discovery file | Whoever holds the token owns the store |

---

## Trust boundaries

```
  operator's system prompt         ← trusted
  ────────────────────────────────────────────
  user turn                        ← semi-trusted (the user's own words)
  ────────────────────────────────────────────
  recalled memory                  ← UNTRUSTED data
  plugin output                    ← UNTRUSTED data
  ────────────────────────────────────────────
```

`graymatter run` renders recalled facts inside a `<memory>` fence with an
explicit note that the contents carry no authority and must never be followed
as instructions. Facts are flattened to one line each and the fence
delimiters are neutralised inside them, so a stored fact cannot close the
block and continue as prompt text.

Framing is mitigation, not a guarantee. Models do sometimes follow instructions
in data. The durable control is limiting who can write a fact in the first
place.

### If you are a library consumer

Do not concatenate `Recall` output straight into a system prompt. Fence it and
label it. The pattern the CLI uses is in
[`internal/harness/memory_prompt.go`](../cmd/graymatter/internal/harness/memory_prompt.go);
the shape is:

```
## Memory (untrusted data)

<one paragraph saying this is data, not instructions>

<memory>
- fact
- fact
</memory>
```

---

## What is defended

| Surface | Control |
|---|---|
| REST API (`graymatter server`) | Binds `127.0.0.1` by default; bearer token on every route but `/healthz`, compared in constant time |
| MCP over HTTP (`mcp serve --http`) | Same bearer token, same loopback default; `--no-auth` refused on non-loopback addresses |
| Daemon RPC | 256-bit token, constant-time compare. Unix: `0600` discovery file + kernel-enforced socket perms. Windows: TCP loopback + the discovery file and the HTTP token file carry a protected owner-only DACL (user + SYSTEM + Administrators), applied at write time and enforced by the kernel — see below |
| Plugin install | Mandatory `sha256`, verified before install and before every call; executable copied inside the store; HTTPS-only manifests; interactive confirmation |
| Plugin / CLI names | Whitelisted identifiers plus a containment check, so no path traversal out of the plugins dir |

---

## What is *not* defended

These are known gaps, stated plainly rather than left for a reader to discover.

**No namespace isolation between agents.** Any client that can authenticate can
read and write *any* `agent_id`. `memory_reflect` takes the agent to modify as a
parameter. One compromised agent can therefore poison another's memory, and a
shared store is a shared trust domain. If you run agents with different trust
levels, give them different data directories.

**No provenance on facts.** A fact does not record who wrote it or where it came
from, so `recall` cannot tell "the user said this" apart from "a web page said
this". Treat the whole store at the trust level of its least trusted writer.

**Same-user processes are inside the boundary.** The daemon's token sits in a
file readable by the user who owns the store, by design. Any process running as
that user can read it and drive the store, including `Shutdown`. GrayMatter is
not a defence against malware already running as you.

`SessionKill` is narrowed anyway: it will only terminate a PID that matches the
PID file graymatter wrote when it spawned that session, so an RPC client cannot
turn a made-up session record into "kill this arbitrary process for me". That
closes the RPC surface as a kill primitive; it does not stop a same-user
process that can write both the record and the file.

**`init` puts the executable's directory on your PATH.** On Windows that is
`HKCU\Environment`. If that directory is writable by anyone else, it becomes a
hijack point for every process that later resolves a command through it. Pass
`--no-path` to skip it, or install into a directory only you can write.

**`0600` is POSIX-only — mitigated for the token files.** On Windows, the
mode passed to `os.WriteFile` does nothing; a file inherits its parent
directory's ACL. Both secret files close that gap explicitly: the daemon's
discovery file and the HTTP bearer-token file receive a *protected* owner-only
DACL at write time (current user + SYSTEM + Administrators, nothing
inherited), so even in a team-shared tree no other local user can read the
token. Failure to apply the DACL aborts the write path instead of running
with an unprotected credential. Remaining Windows caveat: other files in the
data dir (including `gray.db`) still inherit directory ACLs — put the store
in a per-user location if your tree is shared.

**No rate limiting.** An authenticated client can hammer any surface.

**No encryption at rest.** `gray.db` is a plain bbolt file. Disk encryption is
the operating system's job.

---

## Deployment guidance

| Deployment | Verdict |
|---|---|
| One person, one laptop, loopback only | The design point. Fine. |
| Several agents you control, one machine | Fine, but understand they share one trust domain (no namespace isolation). |
| Agents at different trust levels | Give each its own data directory. |
| Store reachable from a network | Keep the bearer token, put TLS in front of it, and treat the port as sensitive. |
| Multi-tenant / untrusted writers | Not supported. There is no per-tenant authorisation. |

---

## Reporting

Security issues: open a GitHub issue, or contact the maintainer privately if the
report includes a working exploit.
