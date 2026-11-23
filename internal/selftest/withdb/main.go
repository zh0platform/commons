// Command withdb is the module's own runner: `go run ./internal/selftest/withdb [--keycloak] [--quiet]
// [--compose <file>]... [--login <name>]... -- <command> [args...]` runs a command against the test product's stack.
// --login adds a login, so naming an existing one makes the start fail.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"git.kloudbox.de/commons/commons/internal/selftest"
	"git.kloudbox.de/commons/commons/testdb"
	"git.kloudbox.de/commons/commons/withdb"
)

const usage = "usage: go run ./internal/selftest/withdb [--keycloak] [--quiet] [--compose <file>]... [--login <name>]... -- <command> [args...]"

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
	os.Exit(run(product, command))
}

func run(product testdb.Product, command []string) int {
	stack, err := withdb.Start(product)
	if err != nil {
		report(err)
		return 1
	}
	status, runErr := stack.Run(command)
	if runErr != nil {
		report(runErr)
	}
	if err := stack.Close(); err != nil {
		report(err)
		return max(status, 1)
	}
	return status
}

func parseArgs(args []string, product *testdb.Product) ([]string, error) {
	for len(args) > 0 {
		switch {
		case args[0] == "--keycloak":
			product.Realms = filepath.Join(product.Root, "internal/selftest/realms")
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

func report(err error) { _, _ = fmt.Fprintln(os.Stderr, "withdb:", err) }

func fail(code int, err error) {
	report(err)
	os.Exit(code)
}
