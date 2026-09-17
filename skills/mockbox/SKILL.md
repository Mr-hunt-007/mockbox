---
name: mockbox
description: Serve a JSON file or an OpenAPI 3 JSON spec as a local mock REST API with mockbox, and check what a request returns. Use when a frontend needs a fake backend, when writing or debugging code that calls a mock API from db.json or openapi.json, or when asked what a mocked endpoint returns.
---

# mockbox

mockbox turns `db.json` (top-level arrays become collections, objects become singular resources) or an OpenAPI 3 JSON spec into a REST API on `http://127.0.0.1:3000`.

## When to use it

- The frontend needs an API before the backend exists.
- You need to know exactly what a mocked request returns: status, headers, body, pagination, errors.
- A test or demo needs a throwaway API from a JSON file.

## Inspect without running a server

If the `mockbox` MCP server is configured, use its tools. They never open a port and never modify the file:

- `mockbox_routes` (`file`): routes, collections, singular resources, relations. Call it first.
- `mockbox_request` (`file`, `method`, `path` with query, optional `body`, `headers`, `routes`): the status, headers and body mockbox would send. Each call starts from the file as it is on disk, so writes are not kept between calls.
- `mockbox_example` (`file`, `method`, `path`, optional `status`, `example`): for OpenAPI specs, the body for a response and whether it came from an example or the schema.

## Running the server

The MCP tools do not serve a browser or app. For that, run mockbox in a terminal (or in the background):

```
mockbox db.json                       # in memory, file never modified
mockbox db.json --port 0 --json       # free port; read "url" from the start event
mockbox db.json --watch --cors        # reload on edit, allow browser origins
mockbox db.json --routes routes.json  # rewrites such as {"/api/*": "/$1"}
mockbox openapi.json                  # static responses from the spec
```

With `--json`, stdout is one JSON object per line:

- `start`: `version`, `file`, `mode` (`database` or `openapi`), `url`, `readonly`, `persist`, `routes`, `rewrites`
- `request`: `time`, `method`, `path`, `rewritten_to` (when rewritten), `status`, `duration_ms`
- `reload`: `time`, `ok`, `message`

## Requests that matter

- `GET /users?role=admin&age_gte=18&_sort=-age` filters and sorts; `?q=text` searches.
- `GET /users?_page=2&_limit=10` adds `X-Total-Count` and `Link` headers.
- `GET /users/1?_embed=posts`, `GET /posts?_expand=user`, `GET /users/1/posts`.
- `POST` returns 201 with `Location`; a duplicate id returns 409.
- OpenAPI: send `Prefer: code=404` or `Prefer: example=name` to pick a response.
- API errors look like `{"error": "Not Found", "status": 404, "message": "..."}`.

## Exit codes

- 0: clean shutdown, `--help`, `--version`, MCP client disconnected
- 1: server could not start (port in use: pick another `--port`)
- 2: invalid flags or arguments
- 3: file unreadable or invalid (bad JSON, YAML given, bad routes file)

## Safety

- The file is only written with `--persist`. Do not add it unless the user wants the file changed.
- The server binds to 127.0.0.1. Only use `--host 0.0.0.0` when asked; there is no authentication.
- YAML specs are not supported; convert to JSON first.
