# Changelog

Versions follow semantic versioning. While the module is `v0`, a breaking change to an exported name is a new minor
version, noted here for its users.

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
