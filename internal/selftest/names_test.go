package selftest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The module is shared by several products: no Go or YAML file of it names one of them. Guards against a product's
// name coming back with a copied fix; it goes when the module's code no longer arrives from a product's copy.
func TestNoModuleFileNamesAProduct(t *testing.T) {
	module := os.DirFS("../..")
	err := fs.WalkDir(module, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if extension := filepath.Ext(path); extension != ".go" && extension != ".yaml" && extension != ".yml" {
			return nil
		}
		if filepath.Base(path) == "names_test.go" { // this file names what it looks for
			return nil
		}
		content, err := fs.ReadFile(module, path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(content)), "bff") {
			t.Errorf("%s names bff", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
