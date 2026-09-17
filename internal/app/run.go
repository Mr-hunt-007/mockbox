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

// Run is the whole command: it parses args, serves until interrupted and
// returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
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

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
