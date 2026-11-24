package withdb_test

import (
	"strings"
	"testing"

	"github.com/zh0platform/commons/internal/selftest"
	"github.com/zh0platform/commons/withdb"
)

// A description a stack cannot be built from fails before any Docker call, naming the field to fix: the runner's own
// fields here, and a database field through testdb's check.
func TestCheckNamesTheFieldThatCannotBuildAStack(t *testing.T) {
	valid := selftest.Product("/work/shop")
	if err := valid.Check(); err != nil {
		t.Fatalf("Check(valid) = %v, want nil", err)
	}
	for field, change := range map[string]func(*withdb.Product){
		"Project":   func(p *withdb.Product) { p.Project = "Shop-test" },
		"Root":      func(p *withdb.Product) { p.Root = "work/shop" },
		"Realms":    func(p *withdb.Product) { p.Realms = "test/realms" },
		"Names":     func(p *withdb.Product) { p.Names = []string{"app.test", "LocalHost."} },
		"Variables": func(p *withdb.Product) { p.Variables = "shop_test_" },
	} {
		broken := valid
		change(&broken)
		if err := broken.Check(); err == nil || !strings.HasPrefix(err.Error(), "field "+field) {
			t.Errorf("Check(product with a bad %s) = %v, want an error naming %s", field, err, field)
		}
	}
}
