package testdb

import (
	"slices"
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

// Each field of the Server travels under its own name, and the command reads back the Server it was handed.
func TestServerTravelsFieldByFieldUnderItsNames(t *testing.T) {
	p := Product{Variables: "SHOP_TEST_"}
	// Five distinct values, one per field; none is shaped like a secret, so the secret scan has nothing to report.
	v := []string{"host", "port", "ca", "first", "second"}
	s := Server{Host: v[0], Port: v[1], CA: v[2], AdminPassword: v[3], LoginPassword: v[4]}
	names := []string{"DB_HOST", "DB_PORT", "DB_CA", "DB_ADMIN_PASSWORD", "DB_LOGIN_PASSWORD"}
	var want []string
	for i, name := range names {
		want = append(want, p.Variables+name+"="+v[i])
	}
	environ := p.ServerEnviron(s)
	if !slices.Equal(environ, want) {
		t.Fatalf("ServerEnviron = %v, want %v", environ, want)
	}
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		t.Setenv(name, value)
	}
	if got, err := p.server(); err != nil || got != s {
		t.Errorf("server() = %+v, %v; want %+v", got, err, s)
	}
}
