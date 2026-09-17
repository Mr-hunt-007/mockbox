# Changelog

## 0.2.0 (2026-09-17)

- `--mcp` runs an MCP server on stdio for AI coding agents, with three read-only tools: `mockbox_routes` (routes, collections, singular resources, relations), `mockbox_request` (status, headers and body of one request against a fresh in-memory copy; the file is never modified) and `mockbox_example` (the example or synthesized body for an OpenAPI operation and where it came from). An optional file argument sets the default file. `--allow-destructive` is accepted and changes nothing, since no tool is destructive.
- `AGENTS.md`, `CLAUDE.md`, `llms.txt` and an Agent Skill in `skills/mockbox/`.

## 0.1.0 (2026-09-17)

First release.

- Serve a JSON file as a REST API: arrays become collections (GET, POST, PUT, PATCH, DELETE), objects become singular resources (GET, PUT, PATCH).
- Filtering with equality, nested paths, `_gte`, `_lte`, `_ne`, `_like` and `q` full text search; `_sort` on several fields; `_page` and `_limit` with `X-Total-Count` and `Link` headers; `_embed` and `_expand`; nested `GET` and `POST` on `/parent/:id/child`.
- Automatic ids (next integer or random string), 409 on duplicate ids, JSON errors with line and column for bad bodies.
- OpenAPI 3 JSON specs: path matching, `example` and `examples`, `Prefer: code=` and `Prefer: example=`, and schema synthesis with `$ref` cycle protection. Clear error for YAML input.
- Flags: `--port`, `--host` (default 127.0.0.1), `--watch`, `--delay`, `--cors`, `--readonly`, `--persist` (atomic writes that keep key order), `--routes`, `--quiet`, `--json`, `--no-color`, `--version`.
- HTML or JSON route index at `/`, request logging, graceful shutdown, clear port-in-use error.
