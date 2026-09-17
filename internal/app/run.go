package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// MCPServer runs the --mcp server on stdin and stdout until stdin closes or
// ctx is cancelled. It is passed in by main so this package does not depend
// on the MCP tools.
type MCPServer func(ctx context.Context, cfg *Config, stdin io.Reader, stdout io.Writer) error

// Run is the whole command: it parses args, serves until interrupted and
// returns the process exit code. serveMCP handles --mcp.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, serveMCP MCPServer) int {
	cfg, err := ParseArgs(args, stderr)
	if errors.Is(err, errHelp) {
		fmt.Fprint(stdout, usageText)
		return ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "mockbox: %v\nRun 'mockbox --help' for usage.\n", err)
		return ExitUsage
	}
	if cfg.Version {
		fmt.Fprintf(stdout, "mockbox %s\n", Version)
		return ExitOK
	}
	if cfg.MCP {
		return runMCP(cfg, stdin, stdout, stderr, serveMCP)
	}
	color := !cfg.NoColor && !cfg.JSON && os.Getenv("NO_COLOR") == "" && isTerminal(stdout)
	a, err := New(cfg, stdout, stderr, color)
	if err != nil {
		fmt.Fprintf(stderr, "mockbox: %v\n", err)
		var ue *UsageError
		if errors.As(err, &ue) {
			return ExitUsage
		}
		return ExitInput
	}
	ln, err := a.Listen()
	if err != nil {
		fmt.Fprintf(stderr, "mockbox: %v\n", err)
		return ExitRuntime
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Serve(ctx, ln); err != nil {
		fmt.Fprintf(stderr, "mockbox: %v\n", err)
		return ExitRuntime
	}
	return ExitOK
}

func runMCP(cfg *Config, stdin io.Reader, stdout, stderr io.Writer, serveMCP MCPServer) int {
	if cfg.File != "" {
		if _, err := os.Stat(cfg.File); err != nil {
			fmt.Fprintf(stderr, "mockbox: cannot read %s: %v\n", cfg.File, unwrapPathError(err))
			return ExitInput
		}
	}
	if cfg.Routes != "" {
		if _, err := os.Stat(cfg.Routes); err != nil {
			fmt.Fprintf(stderr, "mockbox: cannot read routes file: %v\n", err)
			return ExitInput
		}
	}
	if serveMCP == nil {
		fmt.Fprintln(stderr, "mockbox: --mcp is not available in this build")
		return ExitRuntime
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serveMCP(ctx, cfg, stdin, stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "mockbox: %v\n", err)
		return ExitRuntime
	}
	return ExitOK
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
