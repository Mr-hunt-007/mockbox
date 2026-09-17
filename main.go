// Command mockbox turns a JSON file into a working REST API.
package main

import (
	"os"

	"github.com/Mr-hunt-007/mockbox/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:], os.Stdout, os.Stderr))
}
