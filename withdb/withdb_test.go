package withdb

import (
	"slices"
	"strings"
	"testing"

	"git.kloudbox.de/commons/commons/internal/selftest"
	"git.kloudbox.de/commons/commons/testdb"
)

// A description a stack cannot be built from, or an empty command, is refused before any Docker call, with status 1.
func TestRunRefusesBeforeTheStack(t *testing.T) {
	product := selftest.Product(t.TempDir())
	broken := product
	broken.Project = "Not-A-Prefix"
	for name, c := range map[string]struct {
		product testdb.Product
		command []string
	}{"a bad description": {broken, []string{"true"}}, "an empty command": {product, nil}} {
		if status := Run(c.product, c.command); status != 1 {
			t.Errorf("Run(%s) = %d, want 1", name, status)
		}
	}
}

// A product that lists no plaintext logins keeps every login, by name, on the plaintext-prefix databases only (the
// database test pins a listed set against the real server).
func TestPlaintextRuleNamesItsLogins(t *testing.T) {
	product := selftest.Product(t.TempDir())
	product.PlaintextLogins = nil
	want := `hostnossl "/^` + testdb.PlaintextPrefix + `" ` + strings.Join(product.Logins, ",") + " 0.0.0.0/0 scram-sha-256"
	rules := strings.Split(hba(product), "\n")
	if !slices.Contains(rules, want) || strings.Count(hba(product), "hostnossl") != 1 || !slices.Contains(rules, "local all "+testdb.Administrator+" trust") {
		t.Errorf("hba(no plaintext logins) = %q, want the one plaintext rule %q and the administrator's socket", rules, want)
	}
}

// A shell that ran another run of the product, or exported its settings or Compose's own, must not point this run's
// processes at that run's servers or change which services Compose starts; a setting the runner does not manage
// passes through.
func TestInheritedDropsRunnerSettingsComposeAndRefusedVariables(t *testing.T) {
	product := selftest.Product(t.TempDir())
	for _, name := range []string{
		"AGENTTEST_DB_HOST", "AGENTTEST_SERVER_KEY", "AGENTTEST_ROOT", "KEYCLOAK_TEST_URL", "KEYCLOAK_REALMS",
		"COMPOSE_PROFILES", "COMPOSE_FILE", "PGHOST", "AGENT_TYPO", "AGENTTEST_DEVTOOLS",
	} {
		t.Setenv(name, "another-run")
	}
	t.Setenv("AGENTTEST_PG_FLAGS", "-c fsync=on")
	environment := inherited(product)
	for _, entry := range environment {
		if strings.HasSuffix(entry, "=another-run") {
			t.Errorf("inherited(product) keeps %s", entry)
		}
	}
	if !slices.Contains(environment, "AGENTTEST_PG_FLAGS=-c fsync=on") {
		t.Error("inherited(product) lost AGENTTEST_PG_FLAGS, a setting the runner does not manage")
	}
}
