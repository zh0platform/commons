package withdb

import (
	"slices"
	"strings"
	"testing"

	"git.kloudbox.de/commons/commons/internal/selftest"
)

// A shell that ran another run of the product, or exported its settings or Compose's own, must not point this run's
// processes at that run's servers or change which services Compose starts; a setting the runner does not manage
// passes through.
func TestInheritedDropsRunnerSettingsComposeAndRefusedVariables(t *testing.T) {
	product := selftest.Product(t.TempDir())
	for _, name := range []string{
		"AGENTTEST_DB_HOST", "AGENTTEST_SERVER_KEY", "AGENTTEST_ROOT", "KEYCLOAK_TEST_URL", "KEYCLOAK_REALMS",
		"COMPOSE_PROFILES", "COMPOSE_FILE", "PGHOST", "AGENT_TYPO",
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
