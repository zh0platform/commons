package withdb

// The unit tests live outside the package, because the test product they build imports it.
var (
	HBA       = hba
	Inherited = inherited
)

// RunnerVariableNames are the names of every variable the runner sets besides the server's, for a product.
func RunnerVariableNames(p Product) []string {
	var names []string
	for v := range runnerVariableCount {
		names = append(names, v.in(p))
	}
	return names
}
