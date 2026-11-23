package withdb

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"git.kloudbox.de/commons/commons/internal/selftest"
	"git.kloudbox.de/commons/commons/testdb"
)

// A description a stack cannot be built from, or an empty command, is refused before the stack, with status 1 and its
// own one line: a failed start would answer 1 too, with another message.
func TestRunRefusesBeforeTheStack(t *testing.T) {
	product := selftest.Product(t.TempDir())
	broken := product
	broken.Project = "Not-A-Prefix"
	for name, c := range map[string]struct {
		product testdb.Product
		command []string
		printed string
	}{
		"a bad description": {broken, []string{"true"}, "withdb: setup failed: checking the description: field Project"},
		"an empty command":  {product, nil, "withdb: setup failed: no command to run\n"},
	} {
		var status int
		printed := stderrOf(t, func() { status = Run(c.product, c.command) })
		if status != 1 || !strings.HasPrefix(printed, c.printed) || strings.Count(printed, "\n") != 1 {
			t.Errorf("Run(%s) = %d, printed %q; want 1 and one line starting %q", name, status, printed, c.printed)
		}
	}
}

// stderrOf is what run printed on standard error: Run is a runner's entry point, and its errors are its output.
func stderrOf(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() { os.Stderr = original })
	run()
	os.Stderr = original
	_ = writer.Close()
	printed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(printed)
}

// A product that lists no plaintext logins keeps every login, by name, on the plaintext-prefix databases only (the
// database test pins a listed set against the real server).
func TestPlaintextRuleNamesItsLogins(t *testing.T) {
	product := selftest.Product(t.TempDir())
	product.PlaintextLogins = nil
	want := `hostnossl "/^` + testdb.PlaintextPrefix + `" ` + strings.Join(product.Logins, ",") + " 0.0.0.0/0 scram-sha-256"
	rules := hba(product)
	lines := strings.Split(rules, "\n")
	if !slices.Contains(lines, want) || strings.Count(rules, "hostnossl") != 1 || !slices.Contains(lines, "local all "+testdb.Administrator+" trust") {
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
