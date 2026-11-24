package testdb

import (
	"strings"
	"testing"
)

// A description a server cannot be provisioned from fails before any Docker call, naming the field to fix.
func TestCheckNamesTheFieldThatCannotBeProvisioned(t *testing.T) {
	valid := Product{
		Variables: "SHOP_TEST_", Logins: []string{"shop_migrate", "shop_runtime"},
		Template: "shop_template", Schema: "shop", SchemaOwner: "shop_migrate",
		PlaintextLogins: []string{"shop_runtime"},
	}
	if err := valid.Check(); err != nil {
		t.Fatalf("Check(valid) = %v, want nil", err)
	}
	for field, change := range map[string]func(*Product){
		"Variables":       func(p *Product) { p.Variables = "shop_test_" },
		"Logins":          func(p *Product) { p.Logins = nil },
		"SchemaOwner":     func(p *Product) { p.SchemaOwner = "someone_else" },
		"Template":        func(p *Product) { p.Template = "" },
		"Schema":          func(p *Product) { p.Schema = "shop; DROP" },
		"PlaintextLogins": func(p *Product) { p.PlaintextLogins = []string{"shop_other"} },
	} {
		broken := valid
		change(&broken)
		if err := broken.Check(); err == nil || !strings.HasPrefix(err.Error(), "field "+field) {
			t.Errorf("Check(product with a bad %s) = %v, want an error naming %s", field, err, field)
		}
	}
	broken := valid
	broken.Logins, broken.PlaintextLogins = []string{"shop_migrate", "Shop-Runtime"}, nil
	if err := broken.Check(); err == nil || !strings.Contains(err.Error(), "Shop-Runtime") {
		t.Errorf("Check(product with a bad login) = %v, want an error naming the login", err)
	}
}
