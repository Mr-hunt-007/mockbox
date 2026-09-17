# mockbox

Turn a JSON file into a working REST API with one command, so frontend work can start before the backend exists.

```
mockbox db.json
```

## Why

`json-server` is the usual answer, but it needs Node and an npm install in every
project or CI image. mockbox is a single self-contained binary with no runtime
dependencies (it uses only the Go standard library), so you can drop it into any repo, container or CI job. It
also mocks straight from an OpenAPI 3 JSON spec when you have a contract but no data.

It is careful with your data: by default every change stays in memory and the
source file is never touched. With `--persist`, writes go through a temp file and
an atomic rename, and the file keeps its key order.

## Install

```
go install github.com/Mr-hunt-007/mockbox@latest
```

`go install` puts the binary in `$(go env GOPATH)/bin` (usually `~/go/bin`). If your shell says `command not found`, add that directory to your `PATH`:

```sh
echo 'export PATH="$PATH:$(go env GOPATH)/bin"' >> ~/.zshrc && source ~/.zshrc   # bash: ~/.bashrc
```

On Windows the Go installer adds `%USERPROFILE%\go\bin` to `PATH` for you.

Or build from source:

```
git clone https://github.com/Mr-hunt-007/mockbox
cd mockbox
go build -o mockbox .
```

## Usage

```
mockbox <db.json | openapi.json> [flags]
```

Given a `db.json` like this (an excerpt; the full file is [`examples/db.json`](examples/db.json)):

```json
{
  "users": [ { "id": 1, "name": "Ada Lovelace", "role": "admin", "age": 36, "address": { "city": "London" } } ],
  "posts": [ { "id": 1, "title": "Notes on the Analytical Engine", "userId": 1 } ],
  "profile": { "name": "demo", "theme": "dark" }
}
```

top-level arrays become collections and top-level objects become singular resources:

```
$ mockbox db.json
mockbox 0.1.0 serving db.json (database, in memory, file is never modified)

  GET    /users
  GET    /users/:id
  GET    /users/:id/posts
  POST   /users
  PUT    /users/:id
  PATCH  /users/:id
  DELETE /users/:id
  GET    /posts
  GET    /posts/:id
  POST   /posts
  PUT    /posts/:id
  PATCH  /posts/:id
  DELETE /posts/:id
  GET    /profile
  PUT    /profile
  PATCH  /profile

Listening on http://127.0.0.1:3000 (Ctrl-C to stop)
21:25:07 GET    /users?role=admin&_sort=-age 200 0.1ms
21:25:07 GET    /posts?userId=1&_page=1&_limit=1 200 0.3ms
21:25:07 GET    /posts/2?_expand=user 200 0.0ms
21:25:07 POST   /posts 201 0.0ms
21:25:07 POST   /posts 400 0.0ms
```

Top-level keys that are neither arrays nor objects (for example `"version": 3`) are
skipped with a warning.

### Collections

| Request | Result |
| --- | --- |
| `GET /users` | all items, `X-Total-Count` header |
| `GET /users/1` | one item, 404 if missing |
| `GET /users/1/posts` | posts whose `userId` is 1 (query parameters below also apply) |
| `POST /users` | 201, `Location` header. Without an `id`: next integer if every existing id is an integer, otherwise a random 8 character hex string. 409 if the id already exists |
| `POST /users/1/posts` | creates a post with `userId` set to 1 |
| `PUT /users/1` | replaces the item, keeps its id |
| `PATCH /users/1` | shallow merge into the item, `id` cannot change |
| `DELETE /users/1` | 200 with the deleted item |

### Singular resources

`GET /profile`, `PUT /profile` (replace) and `PATCH /profile` (shallow merge).

### Query parameters

| Parameter | Meaning |
| --- | --- |
| `role=admin` | equality. Repeat for OR: `role=admin&role=user`. Numbers compare numerically, so `age=36.0` matches `36`. On an array field, matches if any element is equal |
| `address.city=London` | nested fields with dots (array indexes work too: `tags.0=x`) |
| `age_gte=18`, `age_lte=65` | range, numeric for numbers, string order otherwise (so ISO dates work) |
| `role_ne=admin` | not equal. Items without the field match |
| `title_like=^intro` | case-insensitive regular expression (Go RE2 syntax) |
| `q=turing` | case-insensitive full text search over every string and number in the item |
| `_sort=role,-age` | sort by several fields, `-` for descending. `_order=desc` also works. Items missing the field sort last |
| `_page=2&_limit=10` | pagination. `_limit` defaults to 10 when `_page` is given. Adds `X-Total-Count` and a `Link` header with `first`, `prev`, `next` and `last` |
| `_embed=posts` | on users: adds `posts` whose `userId` equals the user's id |
| `_expand=user` | on posts: adds a `user` object looked up through `userId` |

Relations are found by name: the foreign key for collection `users` is `userId`,
for `categories` it is `categoryId`. Unknown `_embed` or `_expand` targets return
400 rather than silently doing nothing.

```
$ curl -i 'http://localhost:3000/posts?userId=1&_page=1&_limit=1'
HTTP/1.1 200 OK
Content-Type: application/json; charset=utf-8
Link: <http://localhost:3000/posts?_limit=1&_page=1&userId=1>; rel="first", <http://localhost:3000/posts?_limit=1&_page=2&userId=1>; rel="next", <http://localhost:3000/posts?_limit=1&_page=2&userId=1>; rel="last"
X-Total-Count: 2
Date: Thu, 17 Sep 2026 15:55:07 GMT
Content-Length: 119

[
  {
    "id": 1,
    "title": "Notes on the Analytical Engine",
    "userId": 1,
    "published": "1843-09-01"
  }
]
```

```
$ curl -X POST localhost:3000/posts -d '{"title": "oops",}'
{
  "error": "Bad Request",
  "status": 400,
  "message": "invalid JSON body: invalid character '}' looking for beginning of object key string (line 1, column 18)"
}
```

`GET /` shows an HTML list of routes in a browser, or JSON when the `Accept` header
prefers `application/json`. Unknown routes return a JSON 404.

### OpenAPI 3

If the file has a top-level `openapi` key, mockbox serves the spec instead:

```
$ mockbox openapi.json --port 3003
mockbox 0.1.0 serving openapi.json (openapi, static responses from the spec)

  GET    /orders
  POST   /orders
  GET    /orders/{orderId}

  Paths are also served under /v1 (from servers[0].url)

Listening on http://127.0.0.1:3003 (Ctrl-C to stop)
```

```
$ curl http://127.0.0.1:3003/orders
[
  {
    "id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
    "status": "pending",
    "total": 1,
    "createdAt": "2026-01-15T09:30:00Z",
    "customer": {
      "email": "user@example.com"
    },
    "items": [
      {
        "sku": "string",
        "quantity": 1
      }
    ]
  }
]
```

How a response is chosen:

1. The lowest 2xx response, then `2XX`, then `default`. Send `Prefer: code=404` to pick another one.
2. Its `application/json` content (or another `+json` type, or the first type listed).
3. The media type's `examples` (first one, or `Prefer: example=name`), then `example`, then a
   value synthesized from the `schema`.

The synthesizer follows local `$ref`s (with cycle protection: a self-referencing
property is left out, a self-referencing array is empty), honours `example`,
`default`, `const` and `enum`, merges `allOf`, takes the first `oneOf`/`anyOf`,
respects `minimum`/`maximum`, `minItems`/`maxItems` and `minLength`/`maxLength`,
skips `writeOnly` properties, and fills formats such as `date-time`, `date`,
`email`, `uuid`, `uri`, `ipv4` and `ipv6` with fixed, valid values. Output is
deterministic, so snapshots stay stable.

YAML specs are not supported, because the Go standard library has no YAML parser:

```
$ mockbox api.yaml
mockbox: api.yaml: YAML is not supported (mockbox uses only the Go standard library, which has no YAML parser).
Convert the spec to JSON first, for example:
  yq -o=json api.yaml > api.json
  python3 -c 'import sys,yaml,json; json.dump(yaml.safe_load(open(sys.argv[1])), sys.stdout, indent=2)' api.yaml > api.json
then run: mockbox api.json
```

### Rewrites

`--routes routes.json` maps public paths onto the resources. `*` matches anything,
`:name` matches one path segment, and targets use `$1` or `:name`. The first
matching rule wins.

```json
{
  "/api/*": "/$1",
  "/me": "/users/1"
}
```

The request log shows the rewrite, and `Link` and `Location` headers keep the path the client used:

```
21:26:09 GET    /api/users?_page=2&_limit=2 -> /users?_page=2&_limit=2 200 0.1ms
21:26:09 GET    /me -> /users/1 200 0.0ms
```

### Watching the file

With `--watch`, mockbox checks the file every 300 ms and reloads it when it
changes. A broken edit does not take the server down:

```
reloaded db.json
mockbox: reload failed, still serving the last good data: db.json: unexpected end of JSON input (line 3, column 1)
21:26:10 GET    /profile 200 0.0ms
reloaded db.json
```

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--port N` | `3000` | port to listen on, `0` picks a free one |
| `--host ADDR` | `127.0.0.1` | address to bind. Use `0.0.0.0` to expose it on your network |
| `--watch` | off | reload when the file changes (mtime and size polling) |
| `--delay MS` | `0` | wait before answering every request, to see loading states |
| `--cors` | off | CORS headers on every response (the request `Origin` is echoed with credentials allowed, `*` without an `Origin`), 204 for preflight `OPTIONS`, and `X-Total-Count`, `Link`, `Location` exposed |
| `--readonly` | off | `POST`, `PUT`, `PATCH` and `DELETE` return 405 |
| `--persist` | off | write changes back to the file: temp file in the same directory, fsync, atomic rename, permissions kept, 2-space indentation, key order kept. Cannot be combined with `--readonly` or an OpenAPI file |
| `--routes FILE` | none | URL rewrite rules, see above |
| `--quiet` | off | no route table, request log or reload messages. Warnings and errors still go to stderr |
| `--json` | off | startup info, request log and reload events as JSON lines |
| `--no-color` | off | no ANSI colour. Colour is also off when `NO_COLOR` is set or stdout is not a terminal |
| `--version` | | print the version |
| `-h`, `--help` | | usage with examples |

Flags can go before or after the file name.

## JSON shapes

Errors from the API always look like this:

```json
{ "error": "Not Found", "status": 404, "message": "no route for GET /comments" }
```

With `--json`, stdout gets one JSON object per line, each with an `event` field:

```
{"event":"start","version":"0.1.0","file":"db.json","mode":"database","url":"http://127.0.0.1:3005","readonly":true,"persist":false,"routes":[{"method":"GET","path":"/users"},{"method":"GET","path":"/users/:id"},{"method":"GET","path":"/users/:id/posts"},{"method":"GET","path":"/posts"},{"method":"GET","path":"/posts/:id"},{"method":"GET","path":"/profile"}],"rewrites":[]}
{"event":"request","time":"2026-09-17T15:54:46.311057Z","method":"POST","path":"/users","status":405,"duration_ms":0.043}
{"event":"request","time":"2026-09-17T15:54:46.325309Z","method":"GET","path":"/users/2","status":200,"duration_ms":0.229}
```

- `start`: `version`, `file`, `mode` (`database` or `openapi`), `url`, `readonly`, `persist`, `routes` (`method`, `path`), `rewrites` (`from`, `to`).
- `request`: `time` (RFC 3339, UTC), `method`, `path` (as requested, with query), `rewritten_to` (only when a rewrite rule matched), `status`, `duration_ms`.
- `reload`: `time`, `ok` (boolean), `message`. Failed reloads are also written to stderr.

`GET /` with `Accept: application/json` returns `name`, `version`, `file`, `mode`,
`readonly`, `persist` and `routes`.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | clean shutdown (Ctrl-C or SIGTERM), `--help`, `--version` |
| 1 | the server could not start, for example the port is in use |
| 2 | invalid flags or arguments |
| 3 | the input file cannot be read or is invalid (bad JSON, YAML given, bad routes file) |

```
$ mockbox db.json --port 3004
mockbox: port 3004 is already in use on 127.0.0.1; stop the other process or pick another port with --port 3005
```

On Ctrl-C mockbox stops accepting connections and lets in-flight requests finish (up to 5 seconds).

## Limitations

- **PATCH is a shallow merge.** Nested objects in the body replace the stored ones entirely.
- **DELETE does not cascade.** Deleting a user leaves its posts in place.
- **Ids are compared as strings.** `1` and `"1"` are the same id, so `POST` with `"id": "1"` conflicts with `"id": 1`.
- **Relations are guessed from names.** Plural to singular is simple English rules (`users` to `userId`, `categories` to `categoryId`, `boxes` to `boxId`); irregular plurals like `people` are not handled.
- **`--persist` normalises formatting.** Key order and number text are kept, but the whole file is rewritten with 2-space indentation and `\n` line endings, so hand-aligned or one-line objects are reformatted.
- **`--watch` polls.** An edit that keeps the same size and lands within the filesystem's timestamp resolution can be missed; saving again picks it up.
- **OpenAPI mode is stateless.** `POST` does not create anything, request bodies are only checked for JSON syntax (not against the schema), and parameters, headers and security schemes are not validated. External `$ref`s (other files or URLs) are not followed and return a 500 that names the ref. Only `servers[0]` is used for the base path alias. YAML is not supported.
- **Not a json-server clone.** There is no `_start`/`_end`, `_gt`/`_lt`, `_per_page`, `/db` route or static file serving.
- The `Link` header always uses `http://` and the request's `Host` header.
- Request bodies are limited to 10 MB. There is no TLS and no authentication; it is a development tool, which is why it binds to `127.0.0.1` by default.

## License

MIT
