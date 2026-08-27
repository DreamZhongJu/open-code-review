# ocr-acp: standalone ACP server prototype

A reference implementation of the **ACP server adapter** roadmap item
(see `ROADMAP.md` via PR #679 and Issue #674). It exposes OpenCodeReview as a
specialized review agent so any ACP-compatible client (Paseo, Zed, JetBrains
ACP registry) can discover and drive it.

## Design constraints

- **Zero changes to `internal/`.** The server is a separate process that wraps
  the installed `ocr` binary through `os/exec`, matching the agreed principle
  of keeping protocol integration decoupled from the core review engine.
- Standard library only. Protocol parsing is hand-rolled JSON-RPC 2.0 over
  newline-delimited JSON to avoid pinning an SDK before the mentor-facing
  design discussion settles (`coder/acp-go-sdk` vs `ironpark/acp-go`).
- stdout/stderr strictly separated: only ndjson envelopes ever reach stdout;
  child stderr goes to the server's own diagnostics plus bounded tail capture.

## Quick start

```bash
go build ./cmd/ocr-acp

# Deterministic demo without any LLM credentials:
./ocr-acp --ocr mock | cat        # speaks ndjson on stdin/stdout

# Against a real installation:
./ocr-acp --ocr /usr/local/bin/ocr
```

### Client wiring examples

Paseo-style provider registration:

```json
{
  "agents": {
    "opencodereview": {
      "command": "/path/to/ocr-acp",
      "args": ["--transport", "stdio", "--ocr", "/path/to/ocr"]
    }
  }
}
```

Zed `agent_servers` equivalent uses the same command/args pair.

Then in a session:

```
/review --from main --to HEAD
please review the staged changes
```

Both inputs map onto `ocr review --format json --audience agent` with the
workspace from `session/new.cwd`.

## Implemented (v1 baseline)

| Method | Behavior |
|---|---|
| initialize | version negotiation (accepts numeric/string, replies 1), empty `authMethods` |
| authenticate | explicit no-auth reply path |
| session/new | cwd resolution, id minting, immediate `available_commands_update` |
| session/prompt | `/review [--from/--to/--commit/--repo]`, `/scan`, free text -> review; progress chunks every 5s heartbeat + child line passthrough |
| session/cancel | kills child process group (POSIX pgid kill; Windows direct kill) |
| terminal rendering | findings markdown with severity/category/path/L-lines summary |

Testing hooks: `--ocr mock` produces deterministic two-finding output with 3
progress steps, enabling full protocol tests with zero network/model access.

## Known prototype limitations / next steps

1. `--transport http` (Streamable HTTP) returns an explanatory failure; wire
   format per ACP HTTP profile is pending stabilization.
2. Single prompt turn per session at a time; queueing is trivial but deferred.
3. Findings are streamed after process exit because the wrapped CLI emits one
   JSON document; true incremental review streaming lands once
   `ocr delegate/session` outputs stabilize behind a flag.
4. Windows does not yet use Job Objects, so grandchild processes spawned by a
   killed review may linger longer than on POSIX.
5. Real `jsonOutput` field evolution (e.g. SARIF parity) tolerated by lenient
   decoding; strict schema contract discussion belongs in the design phase.

## Files

```
cmd/ocr-acp/
├── main.go            entrypoint & flags (--transport/--ocr/--timeout)
├── acp/types.go       ACP v1 subset structs, stop reasons, error codes
├── acp/conn.go        ndjson framing (mutex-guarded writes)
├── acp/server.go      dispatcher, sessions, prompt lifecycle, rendering
├── wrapper/wrapper.go subprocess supervision: discovery, argv build,
│                      stdout parse/stderr tail, heartbeats, cancellation
├── wrapper/proc_unix.go    posix process-group kill
└── wrapper/proc_windows.go windows fallback kill
```
