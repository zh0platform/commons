package withdb_test

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zh0platform/commons/internal/selftest"
	"github.com/zh0platform/commons/testdb"
	"github.com/zh0platform/commons/withdb"
)

// A description a stack cannot be built from, or an empty command, is refused before the stack, with status 1 and its
// own one line: a failed start would answer 1 too, with another message.
func TestRunRefusesBeforeTheStack(t *testing.T) {
	product := selftest.Product(t.TempDir())
	broken := product
	broken.Project = "Not-A-Prefix"
	for name, c := range map[string]struct {
		product withdb.Product
		command []string
		printed string
	}{
		"a bad description": {broken, []string{"true"}, "withdb: setup failed: checking the description: field Project"},
		"an empty command":  {product, nil, "withdb: setup failed: no command to run\n"},
	} {
		var status int
		printed := stderrOf(t, func() { status = withdb.Run(c.product, c.command) })
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
	rules := withdb.HBA(product)
	lines := strings.Split(rules, "\n")
	if !slices.Contains(lines, want) || strings.Count(rules, "hostnossl") != 1 || !slices.Contains(lines, "local all "+testdb.Administrator+" trust") {
		t.Errorf("hba(no plaintext logins) = %q, want the one plaintext rule %q and the administrator's socket", rules, want)
	}
}

// A shell that ran another run of the product, or exported its settings or Compose's own, must not point this run's
// processes at that run's servers or change which services Compose starts; a setting the runner does not manage
// passes through. The names the runner sets are the ones it filters (runnerNames); the test pins that the known ones
// are among them and that none lacks its name in the table.
func TestInheritedDropsRunnerSettingsComposeAndRefusedVariables(t *testing.T) {
	product := selftest.Product(t.TempDir())
	names := slices.Concat(withdb.RunnerNames(product), []string{"COMPOSE_PROFILES", "COMPOSE_FILE", "PGHOST", "SHOP_TYPO"})
	for _, name := range []string{"SHOPTEST_DB_CA", "SHOPTEST_ADMIN_PASSWORD", "SHOPTEST_SERVER_CERT", "SHOPTEST_SERVER_SPKI",
		"SHOPTEST_PG_HBA", "SHOPTEST_DEVTOOLS", "KEYCLOAK_TEST_URL", "KEYCLOAK_REALMS"} {
		if !slices.Contains(names, name) {
			t.Fatalf("the runner's names %v lack %s", names, name)
		}
	}
	// A variable added to the enumeration without a name in the table would be the bare prefix.
	if slices.Contains(names, product.Variables) {
		t.Fatalf("the runner's names %v hold the bare prefix: a variable has no name in the table", names)
	}
	for _, name := range names {
		t.Setenv(name, "another-run")
	}
	t.Setenv("SHOPTEST_PG_FLAGS", "-c fsync=on")
	environment := withdb.Inherited(product)
	for _, entry := range environment {
		if strings.HasSuffix(entry, "=another-run") {
			t.Errorf("inherited(product) keeps %s", entry)
		}
	}
	if !slices.Contains(environment, "SHOPTEST_PG_FLAGS=-c fsync=on") {
		t.Error("inherited(product) lost SHOPTEST_PG_FLAGS, a setting the runner does not manage")
	}
}
