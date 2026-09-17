# AGENTS.md

## What this is

mockbox is a Go CLI that serves a JSON file as a REST API (top-level arrays become collections, objects become singular resources) or mocks an OpenAPI 3 JSON spec. It binds to 127.0.0.1:3000 by default and keeps changes in memory unless `--persist` is given. `mockbox --mcp` runs an MCP server on stdio with read-only tools for AI agents.

## Layout

- `main.go`: entry point. Calls `app.Run` and passes in `mcptools.Serve` for `--mcp`.
- `internal/app`: flag parsing and help text (`config.go`), the `Run` command and exit codes (`run.go`), the HTTP middleware chain, route index, banner, logging, `--watch` reloads, and `Open` for in-memory use by the MCP tools (`app.go`).
- `internal/db`: the JSON database backend: routing, CRUD, `--persist` atomic writes (`db.go`); filters, sorting, pagination, `Link` headers, relation naming (`query.go`).
- `internal/openapi`: OpenAPI 3 loading, path matching and response selection (`openapi.go`); schema synthesis and `$ref` resolution (`synth.go`).
- `internal/rewrite`: `--routes` URL rewrite rules.
- `internal/watch`: mtime and size polling for `--watch`.
- `internal/jsonx`: ordered JSON objects, parsing with line and column errors, marshalling that keeps key order.
- `internal/httpx`: shared HTTP helpers and the JSON error shape.
- `internal/mcp`: a small stdlib MCP server (JSON-RPC over stdio). Shared with sibling tools; keep it identical to the upstream copy.
- `internal/mcptools`: the mockbox MCP tools (`mockbox_routes`, `mockbox_request`, `mockbox_example`).
- `examples/`: `db.json`, `openapi.json`, `routes.json` used in the README.
- `skills/mockbox/SKILL.md`: Agent Skill for users of the tool.

## Build and test

These are the commands CI runs on Linux, macOS and Windows:

```
gofmt -l .          # must print nothing
go vet ./...
go test -race ./...
go build ./...
```

## Rules for contributors

- Go standard library only. No entries in `go.mod` beyond `module` and `go 1.22`.
- Run `gofmt`. Keep logic in `internal/` packages as testable functions; keep the process and filesystem boundary thin.
- Tests use real fixtures: files written in `t.TempDir()`, requests through `httptest`. Table-driven tests for parsers and query logic. Tests must pass on Windows (use `filepath`, no shell).
- Terminal output in the README is pasted from real runs of the built binary, never typed by hand. Run the binary against realistic files before calling a change done.
- No em dashes in code comments, help text, docs or commit messages.
- The `--json` event shapes, the API error shape `{"error", "status", "message"}` and the MCP tool result shapes are compatibility contracts. Add fields; do not rename or remove them.
- Exit codes are documented and stable: 0 clean shutdown, help, version or MCP client disconnected; 1 server could not start; 2 invalid flags or arguments; 3 input file unreadable or invalid.
- MCP handlers never write to stdout, never call `os.Exit` or `os.Chdir`, and must not write to the source file. Argument problems are returned as tool errors.

## Using mockbox as an agent

- To check what an API returns, prefer `mockbox --mcp`: `mockbox_routes` first, then `mockbox_request` with a method and path. For OpenAPI files, `mockbox_example` shows the body for a given status.
- `--mcp` never opens a port. A frontend, browser or `curl` still needs `mockbox db.json` running in a terminal (use `--port 0` for a free port and `--json` to read the chosen URL from the `start` event).
- With `--json`, stdout is one JSON object per line: `start`, `request` and `reload` events, documented in the README.
- The source file is only modified with `--persist`. Do not add `--persist` unless the user wants the file changed.
