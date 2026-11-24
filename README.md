# commons

Shared Go code of the owner's products, imported as `github.com/zh0platform/commons`. Today it holds test
tooling: a runner that starts one run's own PostgreSQL (and, if asked, Keycloak and Chromium) as a Compose project and
runs a command against it, per-test databases cloned from a template, a PostgreSQL protocol relay for lost-commit
tests, a test certificate authority, the key side of a test identity provider, Keycloak sign-in helpers and
second-factor codes.

**Non-goals.** Anything of one product's own logic: its tables, setting names, tenants or migrations. Secrets or one
deployment's settings. Python or Node tooling. Production code arrives only after its own design.

**What moves in.** Code moves in only when it is the same in several projects for a reason that is not the product.
The owner decides. This module is no place for new code that one project needs: such
code stays in that project until a second one needs the same.

## Packages

| Package | Holds |
|---|---|
| `withdb` | `Run`: starting one run's stack, running the command, removing the stack; the embedded Compose files |
| `testdb` | the product description (`Product`), provisioning, per-test databases, the restore like infrastructure's, the relay |
| `nettest` | a loopback port nothing listens on, the test certificate authority |
| `standin` | an identity provider's keys and key set for tests, and tokens signed with exactly the header the caller states |
| `keycloak` | where the run's Keycloak is, a client that trusts it, what a realm file declares, the sign-in through its form |
| `totp` | RFC 6238 codes |

## Using it

A product describes itself once, in a `testdb.Product` (its prefixes, logins, template, schema, migration step, the
variables it refuses), and writes a small runner `main` that parses its flags and exits with
`withdb.Run(product, command)`; the package documentation of `withdb` states the exit rule. Its tests call `testdb.New`, `NewMigrated` or `NewPlaintext`
with the same `Product`. `internal/selftest` is a complete example: the module's own test product, with the agent's
parameters.

The services are `postgres` (container port 5432), with a realm folder `keycloak` (8443, network alias `idp.test`) and,
with `Browser`, `chrome` (DevTools on 9222, its address handed to the command as `<prefix>DEVTOOLS`; a product's
Compose file may give its `command` list as Chromium flags), all on ephemeral loopback ports. The server certificate names `127.0.0.1`, `::1`, `host.docker.internal`,
`postgres`, `idp.test` and the product's `Names`, never `localhost`. The variables handed to the command and to Compose,
and the server-flag variable a product's own Compose file may set (`<prefix>PG_FLAGS`), are listed in the `withdb`
documentation; they carry the product's prefix.

## Development

`make fix` formats and tidies; `make qa` runs every check: format, vet, lint, `go mod tidy -diff`, the check that no Go,
YAML, shell, TOML or template file names a product (`make names`), the allow lists and their proof (below), the unit,
database and Keycloak tests on one stack under the module's own runner (`make tests`; `make unit` and `make keycloak`
run them apart), the runner as a real process (`make stack`) and govulncheck (`make vuln`). The tests need Docker;
`make install-betterleaks` installs the pinned scanner the allow-list check runs. `make flake TESTS=<regexp>` repeats
tests; `make mutate` runs mutation testing on the lines changed since `origin/main`. `make hooks` turns on the
pre-commit hook, which scans what is staged for secrets and runs the fast checks.

**CI** runs on every push to main and every pull request: `qa` on `ubuntu-24.04` and `ubuntu-24.04-arm`, and the
Secrets scan of every commit since `SCAN_SINCE` (Makefile) on a full clone. A finding prints its rule, file:line and
commit, never the value.

**Allow lists.** A scanner finding is silenced only in `.betterleaks.toml` (secrets) or `.trivyignore.yaml` (kept for the
shared check; the module ships no image). Each entry has an ID, a reason and an expiry, and fails `make qa` from its
expiry date; a Betterleaks entry names one file and one exact value. A real secret is rotated, never listed.

**Updates.** Dependabot proposes Go modules, the workflows' actions and the three Compose images weekly, 7 days after a
release; security updates at once. Grouped minor and patch updates merge by themselves once the checks are green;
majors and Keycloak wait for a person.

**Developing commons together with a product.** The workspace root holds an ignored workspace file, `commons.work`
(with its `commons.work.sum`), that lists the product and `commons`. It is not named `go.work`, so no `go` command
below the workspace picks it up by itself. Name it for the commands that should use the checkout:

```bash
GOWORK=$PWD/../commons.work go test ./...
```

Without it, a product builds against the version its `go.mod` names, as CI does; `make qa` and `go mod tidy -diff`
always test that version. A product's `go.mod` moves only to tags.

**A product's CI and its Linux check before a push** need nothing for commons: no `GOPRIVATE`, no token. The module
comes through proxy.golang.org and is verified by sum.golang.org with Go's default settings.

**Leftovers of killed runs.** A run killed hard (SIGKILL, a cancelled CI job) leaves its project and folder; the next
start of the same product on the same machine prints, for each, a line with the commands that remove it, such as
`docker compose -p <project> down --volumes --remove-orphans --rmi local && rm -rf '<folder>'`. Nothing is removed
automatically.

## Releases

Versions are tags `vMAJOR.MINOR.PATCH`, created only by the organization's admins and never moved or deleted: a wrong
release is followed by a higher version. While the module is `v0`, a breaking change to an exported name is a new
minor version with a note in `CHANGELOG.md` for its users. A release is proven by a product's CI on a candidate tag
(`vX.Y.Z-rc.N`) before the release tag goes on the same commit, whose own CI run is green.

Checked by hand before each release, since Dependabot does not see them, each only 7 days after its own release:
gomutants (Makefile), golangci-lint (`scripts/check-tools.sh`), Betterleaks and govulncheck (Makefile) and the Go
toolchain (Makefile and `.github/workflows/ci.yml`).

## Guarantees

- **cleanup**: Runs. A run removes its Compose project (containers, network, volumes, locally built images) and its
  folder on every exit it makes itself: the command's end, a command that could not start, a failure before the
  command, a failed start, INT and TERM; a failed removal keeps the folder for the next start to find. Not after a hard
  kill: what the next start of the same product finds on the same Docker daemon, from the same machine and the same
  temporary folder, it names with its removal commands; Docker objects made before the first container and folders
  under another temporary folder are not found. Tests: `withdb/stack_test.go::TestRunnerRemovesWhatItStartedAndNothingElse`
- **ownership**: A run changes only the project and folder it created; leftovers of other runs are reported, never
  removed. Shown across processes, since the package allows one run per process. Tests: `withdb/stack_test.go::TestRunnerRemovesWhatItStartedAndNothingElse`
- **trust**: Settings. The command gets this run's settings only (its addresses, the DevTools address, passwords and
  certificate material); no inherited setting of another run of the product, no inherited Compose setting and no
  inherited variable the product refuses reaches it. Tests: `withdb/withdb_test.go::TestInheritedDropsRunnerSettingsComposeAndRefusedVariables`, `withdb/stack_test.go::the command's environment`
- **trust**: Secrets. The login password and the server key are in no host file the module writes, in none of its
  output, and in no server log line from provisioning, whatever logging flags a product gives the server. Tests: `withdb/stack_test.go::failed start, statements logged`
- **trust**: Plaintext. Over TCP without TLS the server admits only the product's plaintext logins (every product login
  when it lists none), and only on the plaintext-prefix databases; every other connection needs TLS (the
  administrator's socket inside the container is unchanged). Tests:
  `withdb/withdb_test.go::TestPlaintextRuleNamesItsLogins`,
  `testdb/testdb_database_test.go::TestPlaintextAdmitsOnlyTheListedLogins`
- **trust**: Tokens. A stand-in token's protected header is exactly the members its caller stated plus `alg` (and the
  stand-in's key ID when its own key signs), each replacing a caller's member of that name; nothing adds a `typ`, and
  the caller's header is not changed. The stand-in's private keys never leave it: the key set it generates holds public keys only.
  Tests: `standin/standin_test.go::TestTokenHeaderIsTheCallersHeader`,
  `standin/standin_test.go::TestStandInServesAnAccessTokenUser`
- **cleanup**: Tests. A test database and a relay end with the test that made them: the database is dropped and every
  connection the relay accepted is closed, also one blocked writing to a server that stopped reading. Tests:
  `testdb/testdb_database_test.go::TestDatabaseAndRelayEndWithTheirTest`,
  `testdb/forwarder_test.go::TestRelayToAServerThatStoppedReadingEndsWithItsTest`
- **error mapping**: Restore programs. `RestoreLikeInfra`'s server-side programs get no rows, so none can fail the
  restore because it did not read its input; a program that fails fails the test, naming it. Tests:
  `testdb/testdb_database_test.go::TestServerProgramsAreJudgedByTheirExitStatus`
- **trust**: Secrets in commits. A push to main or a pull request fails at the Secrets workflow when a commit since
  `SCAN_SINCE` adds a secret the scanner recognises, in an added, a modified or a merge commit's file. Tests:
  `scripts/check-allowlists.sh::probe`
