package testdb

import (
	"strings"
	"testing"
)

// A description a stack cannot be built from fails before any Docker call, naming the field to fix.
func TestCheckNamesTheFieldThatCannotBuildAStack(t *testing.T) {
	valid := Product{
		Project: "shop-test-", Variables: "SHOP_TEST_", Logins: []string{"shop_migrate", "shop_runtime"},
		Template: "shop_template", Schema: "shop", SchemaOwner: "shop_migrate", Root: "/work/shop",
	}
	if err := valid.Check(); err != nil {
		t.Fatalf("Check(valid) = %v, want nil", err)
	}
	for field, change := range map[string]func(*Product){
		"Project":         func(p *Product) { p.Project = "Shop-test" },
		"Variables":       func(p *Product) { p.Variables = "shop_test_" },
		"Logins":          func(p *Product) { p.Logins = nil },
		"SchemaOwner":     func(p *Product) { p.SchemaOwner = "someone_else" },
		"Root":            func(p *Product) { p.Root = "work/shop" },
		"Template":        func(p *Product) { p.Template = "" },
		"Schema":          func(p *Product) { p.Schema = "shop; DROP" },
		"Realms":          func(p *Product) { p.Realms = "test/realms" },
		"Names":           func(p *Product) { p.Names = []string{"app.test", "LocalHost."} },
		"PlaintextLogins": func(p *Product) { p.PlaintextLogins = []string{"shop_runtime", "shop_other"} },
	} {
		broken := valid
		change(&broken)
		if err := broken.Check(); err == nil || !strings.HasPrefix(err.Error(), "field "+field) {
			t.Errorf("Check(product with a bad %s) = %v, want an error naming %s", field, err, field)
		}
	}
	broken := valid
	broken.Logins = []string{"shop_migrate", "Shop-Runtime"}
	if err := broken.Check(); err == nil || !strings.Contains(err.Error(), "Shop-Runtime") {
		t.Errorf("Check(product with a bad login) = %v, want an error naming the login", err)
	}
}
