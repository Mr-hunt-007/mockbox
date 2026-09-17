// Command mockbox turns a JSON file into a working REST API.
package main

import (
	"os"

	"github.com/Mr-hunt-007/mockbox/internal/app"
	"github.com/Mr-hunt-007/mockbox/internal/mcptools"
)

func main() {
	os.Exit(app.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, mcptools.Serve))
}
