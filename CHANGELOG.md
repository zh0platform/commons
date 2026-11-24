# Changelog

Versions follow semantic versioning. While the module is `v0`, a breaking change to an exported name is a new minor
version, noted here for its users.

## v0.4.0

Breaking, for every product on the module (bff follows in its switch; the agent and Membership in their own stories):

- The runner's fields left `testdb.Product` for the new `withdb.Product`, which embeds a `testdb.Product`: `Project`,
  `Names`, `Refused`, `Root`, `Realms`, `Files`, `Browser` and `Quiet` moved unchanged. `withdb.Run` takes a
  `withdb.Product`; `withdb.Product.Check` checks the database fields (through `testdb.Product.Check`), then its own.
  Migration: keep the database fields in a `testdb.Product` for the tests, and build
  `withdb.Product{Product: <it>, Project: …, Root: …}` in the runner.

## v0.3.0

Breaking, for every product on the module (bff follows in its switch; the agent and Membership start on v0.3):

- The module path is `github.com/zh0platform/commons`: a public repository, fetched through proxy.golang.org with Go's
  default settings, with no `GOPRIVATE`, token or integration. A product changes its import lines and its `go.mod`.
  The tags up to v0.2.0 stay in the repository as history; under the new path the Go command refuses them, since
  their `go.mod` names the old path.
- `testdb`: `RestoreLikeInfra` dumps without the rows of unlogged tables (`--no-unlogged-table-data`), as
  infrastructure's dump does. An unlogged table arrives in the copy empty.

Fixed:

- `testdb`: `RestoreLikeInfra`'s server-side programs (pg_dump, pg_restore, the dump's removal) get no rows, so a
  program that exits without reading its input can no longer fail the restore with a broken pipe. A failing program
  still fails the test, naming it.

Additive:

- `standin`: `StandIn.Profile`, the profile a stand-in was made with.

Internal: the embedded Compose files are named `compose.<service>.yaml`.

## v0.2.0

Breaking, for every product on the module (bff follows in its switch; the agent and Membership start on v0.2):

- `withdb`: `Start` and the type `Stack` with its methods `Published`, `Run` and `Close` are gone. A runner `main` parses its flags and exits
  with `withdb.Run(product, command)`, which prints its errors (each line after `withdb: `) and answers the exit
  status by the same rule as before; an empty command is refused before the stack comes up.
- `testdb`: `ClosedPort`, `Authority` (with its methods `PEM` and `ServerCertificate`) and `NewAuthority` moved, unchanged,
  to the new package `nettest`.
- `withdb`: `<prefix>DEVTOOLS` is no longer inherited by any run; with `Product.Browser` the service name `chrome`
  belongs to the module, and a product file's `chrome` block merges into it (give flags as its `command` list only).
- `keycloak`: a `Realm` client holds its `RedirectURIs` slice, so two client values no longer compare with `==`.

Additive:

- `standin`: an identity provider's keys and key set for tests (keycloak and auth0 paths, rotation, failure, held and
  replaced answers, fetch counts) and tokens with exactly the caller's header. Its held fetch answers with the state at
  its release; a stand-in that answered with the state before the wait behaves differently for a test that changes
  the state during a hold.
- `testdb.Product.Browser` and `testdb.Product.PlaintextLogins`; `Forwarder.DropAnswerAfter(tag, skip)`;
  `keycloak.Realm` clients' `RedirectURIs`. The module requires `github.com/go-jose/go-jose/v4`.
- `withdb`: provisioning has a bound of its own instead of what is left of the start's, so an expired one is named
  truthfully. A test pins the relay's cleanup against a server that stopped reading.

## v0.1.1

Fixes, no change to an exported name:

- `withdb`: a run removes a folder only once it made it; a failure to make it leaves everything as it was. The leftover
  report's Compose call gets the run's filtered environment and ends on a stop signal. A setup error names the bound
  that expired (start, provisioning or a port lookup) instead of always the start bound. Each bound states how it was
  measured.
- `testdb`: `Check` refuses `localhost` among `Names` in any case and with a final dot, and a relative `Realms`. The
  relay's cleanup closes the server side of each connection too, so a relay blocked writing to a server that stopped
  reading ends.
- `RestoreLikeInfra` (in `v0.1.0` already, noted here): the dump's removal reads the copied row first, so it no longer
  races the server's write (a broken pipe).

## v0.1.0

The database test tooling, moved from bff's `test/` and freed of every product name:

- `withdb`: a library around one run's stack (`Start`, `Stack.Published`, `Stack.Run`, `Stack.Close`) instead of a
  command; a product keeps a small runner `main` with its flags. Compose files ship embedded and are written to a
  per-run folder. Every Compose call and the command get the inherited environment without the product's refused
  variables, without Compose's own and without the runner's names. Leftover reports cover run folders too.
- `testdb`: a `Product` describes a product (prefixes, logins, statements, template, schema, migration step, refused
  variables, Compose files); `Check` validates it before any Docker call. Settings come back as plain `Settings`.
  Provisioning turns off every statement-logging setting. `RestoreLikeInfra` takes the logins that get CONNECT. The
  relay's message cap is 1 MiB; a connection that ends either way closes both sockets; the test's cleanup closes every
  accepted connection.
- `keycloak`, `totp`: as they were.
