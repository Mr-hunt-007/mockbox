package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// Version is the mockbox release.
const Version = "0.1.0"

// Exit codes.
const (
	ExitOK      = 0 // clean shutdown, --help, --version
	ExitRuntime = 1 // server could not start (port in use, bad host)
	ExitUsage   = 2 // bad flags or arguments
	ExitInput   = 3 // source, spec or routes file unreadable or invalid
)

// Config holds parsed command line options.
type Config struct {
	File     string
	Port     int
	Host     string
	Watch    bool
	Delay    int
	CORS     bool
	Readonly bool
	Persist  bool
	Routes   string
	Quiet    bool
	JSON     bool
	NoColor  bool
	Version  bool
	Help     bool
}

const usageText = `mockbox: turn a JSON file into a working REST API.

Usage:
  mockbox <db.json | openapi.json> [flags]

Top-level arrays in db.json become collections (GET/POST /users,
GET/PUT/PATCH/DELETE /users/:id), objects become singular resources
(GET/PUT/PATCH /profile). A file with an "openapi" key is served as an
OpenAPI 3 mock instead.

Flags:
  --port N          port to listen on (default 3000, 0 picks a free port)
  --host ADDR       address to bind (default 127.0.0.1)
  --watch           reload when the file changes; a broken edit keeps the last good data
  --delay MS        wait MS milliseconds before answering each request
  --cors            send CORS headers and answer preflight OPTIONS requests
  --readonly        reject POST, PUT, PATCH and DELETE with 405
  --persist         write changes back to the file (default: in memory only)
  --routes FILE     URL rewrites, e.g. {"/api/*": "/$1"}
  --quiet           do not print the route table or request log
  --json            print the startup info and request log as JSON lines
  --no-color        disable colour (also honours NO_COLOR)
  --version         print the version and exit
  -h, --help        show this help

Query parameters (collections):
  ?role=admin  ?address.city=Paris  ?price_gte=10  ?price_lte=20
  ?status_ne=done  ?title_like=^intro  ?q=search  ?_sort=name,-age
  ?_page=2&_limit=10  ?_embed=posts  ?_expand=user

Examples:
  mockbox db.json
  mockbox db.json --port 4000 --watch --cors
  mockbox db.json --persist --delay 300
  mockbox db.json --routes routes.json --readonly
  mockbox openapi.json --port 8080

Exit codes:
  0  clean shutdown (Ctrl-C), --help, --version
  1  server could not start (port in use, cannot bind)
  2  invalid flags or arguments
  3  input file unreadable or invalid (JSON error, YAML given, bad routes file)
`

// errHelp signals that help was printed.
var errHelp = errors.New("help requested")

// ParseArgs parses flags, allowing them before or after the file argument.
func ParseArgs(args []string, stderr io.Writer) (*Config, error) {
	cfg := &Config{}
	fs := flag.NewFlagSet("mockbox", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVar(&cfg.Port, "port", 3000, "")
	fs.StringVar(&cfg.Host, "host", "127.0.0.1", "")
	fs.BoolVar(&cfg.Watch, "watch", false, "")
	fs.IntVar(&cfg.Delay, "delay", 0, "")
	fs.BoolVar(&cfg.CORS, "cors", false, "")
	fs.BoolVar(&cfg.Readonly, "readonly", false, "")
	fs.BoolVar(&cfg.Persist, "persist", false, "")
	fs.StringVar(&cfg.Routes, "routes", "", "")
	fs.BoolVar(&cfg.Quiet, "quiet", false, "")
	fs.BoolVar(&cfg.JSON, "json", false, "")
	fs.BoolVar(&cfg.NoColor, "no-color", false, "")
	fs.BoolVar(&cfg.Version, "version", false, "")
	fs.BoolVar(&cfg.Help, "help", false, "")
	fs.BoolVar(&cfg.Help, "h", false, "")

	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return cfg, errHelp
			}
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	if cfg.Help {
		return cfg, errHelp
	}
	if cfg.Version {
		return cfg, nil
	}
	switch len(positional) {
	case 0:
		return nil, errors.New("missing file argument: mockbox <db.json | openapi.json>")
	case 1:
		cfg.File = positional[0]
	default:
		return nil, fmt.Errorf("expected one file argument, got %d: %s", len(positional), strings.Join(positional, " "))
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("--port must be between 0 and 65535, got %d", cfg.Port)
	}
	if cfg.Delay < 0 {
		return nil, fmt.Errorf("--delay must be 0 or more milliseconds, got %d", cfg.Delay)
	}
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("--host must not be empty")
	}
	if cfg.Persist && cfg.Readonly {
		return nil, errors.New("--persist and --readonly cannot be used together")
	}
	return cfg, nil
}
