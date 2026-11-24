// Command withdb is the module's own runner: `go run ./internal/selftest/withdb [--keycloak] [--browser] [--quiet]
// [--compose <file>]... [--login <name>]... -- <command> [args...]` runs a command against the test product's stack.
// --login adds a login, so naming an existing one makes the start fail.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zh0platform/commons/internal/selftest"
	"github.com/zh0platform/commons/withdb"
)

const usage = "usage: go run ./internal/selftest/withdb [--keycloak] [--browser] [--quiet] [--compose <file>]... [--login <name>]... -- <command> [args...]"

func main() {
	root, err := os.Getwd()
	if err != nil {
		fail(1, err)
	}
	product := selftest.Product(root)
	command, err := parseArgs(os.Args[1:], &product)
	if err != nil {
		fail(2, err)
	}
	os.Exit(withdb.Run(product, command))
}

func parseArgs(args []string, product *withdb.Product) ([]string, error) {
	for len(args) > 0 {
		switch {
		case args[0] == "--keycloak":
			product.Realms = filepath.Join(product.Root, "internal/selftest/realms")
		case args[0] == "--browser":
			product.Browser = true
		case args[0] == "--quiet":
			product.Quiet = true
		case args[0] == "--compose" && len(args) > 1:
			product.Files = append(product.Files, args[1])
			args = args[1:]
		case args[0] == "--login" && len(args) > 1:
			product.Logins = append(product.Logins, args[1])
			args = args[1:]
		case args[0] == "--" && len(args) > 1:
			return args[1:], nil
		default:
			return nil, errors.New(usage)
		}
		args = args[1:]
	}
	return nil, errors.New(usage)
}

func fail(code int, err error) {
	_, _ = fmt.Fprintln(os.Stderr, "withdb:", err)
	os.Exit(code)
}
