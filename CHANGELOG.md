# Changelog

Versions follow semantic versioning. While the module is `v0`, a breaking change to an exported name is a new minor
version, noted here for its users.

## Unreleased

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
