// Package selftest is the module's own test product, a shop: a user of the tooling shaped like a real product (its
// prefixes, four logins of which one bypasses row security, a role without login granted with SET and without
// INHERIT, its schema, and the SHOP_ family it refuses), so the module's tests build a stack that names no product.
package selftest

import (
	"context"
	"strings"
	"testing"

	"github.com/zh0platform/commons/testdb"
	"github.com/zh0platform/commons/withdb"
)

// The logins and the role the test product provisions.
const (
	MigrateLogin   = "shop_migrate"
	RuntimeLogin   = "shop_runtime"
	LifecycleLogin = "shop_lifecycle"
	RecoveryLogin  = "shop_recovery"
	SchedulerRole  = "shop_scheduler"
)

// Product is the test product rooted at the module's folder root.
func Product(root string) withdb.Product {
	return withdb.Product{
		Variables: "SHOPTEST_",
		Logins:    []string{MigrateLogin, RuntimeLogin, LifecycleLogin, RecoveryLogin},
		// The login that bypasses row security connects only with TLS.
		PlaintextLogins: []string{MigrateLogin, RuntimeLogin, LifecycleLogin},
		Statements: []string{
			"ALTER ROLE " + RecoveryLogin + " BYPASSRLS",
			"CREATE ROLE " + SchedulerRole + " NOLOGIN",
			"GRANT " + SchedulerRole + " TO " + MigrateLogin + " WITH INHERIT FALSE, SET TRUE",
		},
		Template:    "shop_template",
		Schema:      "shop",
		SchemaOwner: MigrateLogin,
		Migrate:     migrate,
		Project:     "shop-test-",
		Names:       []string{"shop.test"},
		Refused:     refused,
		Root:        root,
	}
}

// migrate creates the test product's one table, as its migration login.
func migrate(t testing.TB, d *testdb.Database) {
	t.Helper()
	conn := d.Connect(t, MigrateLogin, d.LoginsPassword())
	if _, err := conn.Exec(context.Background(), "CREATE TABLE shop.marker (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// refused is the shop's rule: the variables a library reads on its own, and its own family.
func refused(name string) bool {
	return name == "DATABASE_URL" || strings.HasPrefix(name, "PG") || strings.HasPrefix(name, "SHOP_")
}
