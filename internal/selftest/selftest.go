// Package selftest is the module's own test product: a second user of the tooling with the agent's parameters (its
// prefixes, four logins of which one bypasses row security, a role without login granted with SET and without
// INHERIT, its schema, and the AGENT_ family it refuses), so the module's tests build a stack that names no product
// of its own.
package selftest

import (
	"context"
	"strings"
	"testing"

	"git.kloudbox.de/commons/commons/testdb"
)

// The logins and the role the test product provisions.
const (
	MigrateLogin   = "agent_migrate"
	RuntimeLogin   = "agent_runtime"
	LifecycleLogin = "agent_lifecycle"
	RecoveryLogin  = "agent_recovery"
	SchedulerRole  = "agent_scheduler"
)

// Product is the test product rooted at the module's folder root.
func Product(root string) testdb.Product {
	return testdb.Product{
		Project:   "agent-test-",
		Variables: "AGENTTEST_",
		Logins:    []string{MigrateLogin, RuntimeLogin, LifecycleLogin, RecoveryLogin},
		// The login that bypasses row security connects only with TLS.
		PlaintextLogins: []string{MigrateLogin, RuntimeLogin, LifecycleLogin},
		Statements: []string{
			"ALTER ROLE " + RecoveryLogin + " BYPASSRLS",
			"CREATE ROLE " + SchedulerRole + " NOLOGIN",
			"GRANT " + SchedulerRole + " TO " + MigrateLogin + " WITH INHERIT FALSE, SET TRUE",
		},
		Template:    "agent_template",
		Schema:      "agent",
		SchemaOwner: MigrateLogin,
		Names:       []string{"agent.test"},
		Migrate:     migrate,
		Refused:     refused,
		Root:        root,
	}
}

// migrate creates the test product's one table, as its migration login.
func migrate(t testing.TB, d *testdb.Database) {
	t.Helper()
	conn := d.Connect(t, MigrateLogin, d.LoginsPassword())
	if _, err := conn.Exec(context.Background(), "CREATE TABLE agent.marker (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// refused is the agent's rule: the variables a library reads on its own, and its own family.
func refused(name string) bool {
	return name == "DATABASE_URL" || strings.HasPrefix(name, "PG") || strings.HasPrefix(name, "AGENT_")
}
