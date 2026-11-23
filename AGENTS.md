# Agent instructions

What the module holds, how to run it and what it promises are in `README.md`. These are the rules that are not obvious
from the code.

## Before finishing

Run `make fix`, then `make qa`. New or changed tests must also pass `make flake TESTS=<pattern>`.

## What goes here

- Only code that is the same in several projects for a reason that is not the product, after the owner's decision
  (README, "What moves in"). No product's name, tables, settings or logic: a product describes itself in a
  `testdb.Product`; `make names` (part of `make qa`) fails on a product's name in any Go or YAML file.
- An exported name is a contract with every product: change one only with a new minor version and a note in
  `CHANGELOG.md`, and say which products must follow.

## Tests

- Unit tests have no tag. `//go:build database` tests run under the module's own runner (`make unit`) against the test
  product in `internal/selftest`; `//go:build stack` tests start runners themselves (`make stack`); `//go:build
  keycloak` tests run with the module's Keycloak (`make keycloak`).
- A test never skips; a missing server is a setup failure. Wait on a condition with a deadline; end everything a test
  starts in its own cleanup.
- Each guarantee in the README names the tests that pin it; a change to a promise changes the list in the same commit.
