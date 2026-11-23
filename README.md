# commons

Shared Go code of the owner's products, imported as `git.kloudbox.de/commons/commons` (private). Today it holds the
database test tooling: a runner that starts one run's own PostgreSQL (and, if asked, Keycloak) as a Compose project
and runs a command against it, per-test databases cloned from a template, a PostgreSQL protocol relay for lost-commit
tests, a test certificate authority, Keycloak sign-in helpers and second-factor codes.

**Non-goals.** Anything of one product's own logic: its tables, setting names, tenants or migrations. Secrets or one
deployment's settings. Python or Node tooling. Production code arrives only after its own design.

**What moves in.** Code moves in only when it is the same in several projects for a reason that is not the product.
The owner decides, through the Commons Elephant. This module is no place for new code that one project needs: such
code stays in that project until a second one needs the same.

## Packages

| Package | Holds |
|---|---|
| `withdb` | starting one run's stack, running the command, removing the stack; the embedded Compose files |
| `testdb` | the product description (`Product`), provisioning, per-test databases, the restore like infrastructure's, the relay, the test authority |
| `keycloak` | where the run's Keycloak is, a client that trusts it, what a realm file declares, the sign-in through its form |
| `totp` | RFC 6238 codes |

## Using it

A product describes itself once, in a `testdb.Product` (its prefixes, logins, template, schema, migration step, the
variables it refuses), and writes a small runner `main` around `withdb.Start`, `Stack.Run` and `Stack.Close`; the
package documentation of `withdb` states the exit rule. Its tests call `testdb.New`, `NewMigrated` or `NewPlaintext`
with the same `Product`. `internal/selftest` is a complete example: the module's own test product, with the agent's
parameters.

The services are `postgres` (container port 5432) and, with a realm folder, `keycloak` (8443, network alias
`idp.test`), both on ephemeral loopback ports. The server certificate names `127.0.0.1`, `::1`, `host.docker.internal`,
`postgres`, `idp.test` and the product's `Names`, never `localhost`. The variables handed to the command and to Compose,
and the server-flag variable a product's own Compose file may set (`<prefix>PG_FLAGS`), are listed in the `withdb`
documentation; they carry the product's prefix.

## Development

`make fix` formats and tidies; `make qa` runs every check: format, vet, lint, `go mod tidy -diff`, the check that no Go
or YAML file names a product (`make names`), the unit, database and Keycloak tests on one stack under the module's own
runner (`make tests`; `make unit` and `make keycloak` run them apart), and the runner as a real process (`make
stack`). The tests need Docker. `make flake TESTS=<regexp>` repeats tests; `make mutate`
runs mutation testing on the lines changed since `origin/main`.

**Developing commons together with a product.** The workspace root holds an ignored workspace file, `commons.work`
(with its `commons.work.sum`), that lists the product and `commons`. It is not named `go.work`, so no `go` command
below the workspace picks it up by itself. Name it for the commands that should use the checkout:

```bash
GOWORK=$PWD/../commons.work go test ./...
```

Without it, a product builds against the version its `go.mod` names, as CI does; `make qa` and `go mod tidy -diff`
always test that version. A product's `go.mod` moves only to tags.

**A product's Linux check before a push** runs in a container without credentials: download the module on the Mac
first (`go mod download`), then mount the Mac's module download cache read-only into the container as a file proxy,
for example `-v "$(go env GOMODCACHE)/cache/download:/proxy:ro" -e GOPROXY=file:///proxy -e GOFLAGS=-mod=readonly`.
`go.sum` verifies the module; no `GOPRIVATE` inside the container.

**A product's CI** sets `GOPRIVATE=git.kloudbox.de/commons/*` in its Makefile and in each job, and fetches the module in
one step before anything compiles, with no stored secret:

1. The owner creates, for the product's repository, a Forgejo "authorized integration" (Settings, Authorized
   Integrations, type Forgejo Actions (Local)): source repository the product's, capability `repository: Read` on
   `Commons/commons` only. Its audience is not a secret.
2. The workflow sets `enable-openid-connect: true`. The step first runs `go mod download git.kloudbox.de/commons/commons`
   without a credential (a warm module cache answers); on a miss it fetches the job's token
   (`$ACTIONS_ID_TOKEN_REQUEST_URL&audience=<audience>` with `Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN`)
   and repeats the download with `GIT_CONFIG_COUNT=1`, `GIT_CONFIG_KEY_0=http.https://git.kloudbox.de/commons/commons.git.extraheader`
   and `GIT_CONFIG_VALUE_0="Authorization: Bearer <token>"` in that command's environment only. The key ends in `.git`:
   git matches a URL prefix at slash boundaries. bff's `scripts/fetch-commons.sh` is a complete example.

**Leftovers of killed runs.** A run killed hard (SIGKILL, a cancelled CI job) leaves its project and folder; the next
start of the same product on the same machine prints, for each, a line with the commands that remove it, such as
`docker compose -p <project> down --volumes --remove-orphans --rmi local && rm -rf '<folder>'`. Nothing is removed
automatically.

## Releases

Versions are tags `vMAJOR.MINOR.PATCH`, protected and never moved. While the module is `v0`, a breaking change to an
exported name is a new minor version with a note in `CHANGELOG.md` for its users. A release is proven by a product's
CI on a candidate tag (`vX.Y.Z-rc.N`) before the release tag goes on the same commit.

## Guarantees

- **cleanup**: Runs. A run removes its Compose project (containers, network, volumes, locally built images) and its
  folder on every exit it makes itself: the command's end, a command that could not start, a failure before the
  command, a failed start, INT and TERM; a failed removal keeps the folder for the next start to find. Not after a hard
  kill: what the next start of the same product finds on the same Docker daemon, from the same machine and the same
  temporary folder, it names with its removal commands; Docker objects made before the first container and folders
  under another temporary folder are not found. Tests: `withdb/stack_test.go::TestRunnerRemovesWhatItStartedAndNothingElse`
- **ownership**: A run changes only the project and folder it created; leftovers of other runs are reported, never
  removed. Shown across processes, since the package allows one stack per process. Tests: `withdb/stack_test.go::TestRunnerRemovesWhatItStartedAndNothingElse`
- **trust**: Settings. The command gets this run's settings only (its addresses, passwords and certificate material);
  no inherited setting of another run of the product, no inherited Compose setting and no inherited variable the
  product refuses reaches it; the caller's own further variables are trusted input. Tests: `withdb/withdb_test.go::TestInheritedDropsRunnerSettingsComposeAndRefusedVariables`, `withdb/stack_test.go::the command's environment`
- **trust**: Secrets. The login password and the server key are in no host file the module writes, in none of its
  output, and in no server log line from provisioning, whatever logging flags a product gives the server. Tests: `withdb/stack_test.go::failed start, statements logged`
- **cleanup**: Tests. A test database and a relay end with the test that made them: the database is dropped and every
  connection the relay accepted is closed. Tests: `testdb/testdb_database_test.go::TestDatabaseAndRelayEndWithTheirTest`
