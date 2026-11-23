package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"git.kloudbox.de/commons/bff/test/browser"
	"git.kloudbox.de/commons/bff/test/keycloak"
	"git.kloudbox.de/commons/bff/test/testdb"
)

// A shell that ran another withdb, or exported its settings, must not point this run's command at that run's servers:
// a run without Keycloak would otherwise reach another run's Keycloak instead of failing.
func TestCommandEnvironGetsNoInheritedRunnerSettings(t *testing.T) {
	for _, name := range []string{keycloak.URLVariable, testdb.HostVariable, browser.DevToolsVariable, "PGHOST"} {
		t.Setenv(name, "another-run")
	}
	t.Setenv("BFF_TEST_PG_FLAGS", "-c fsync=on") // a setting withdb does not manage
	environment := commandEnviron()
	for _, entry := range environment {
		if strings.HasSuffix(entry, "=another-run") {
			t.Errorf("the command inherits %s", entry)
		}
	}
	if !slices.Contains(environment, "BFF_TEST_PG_FLAGS=-c fsync=on") {
		t.Error("the command lost an inherited setting withdb does not manage")
	}
}

// A dropped --compose file would run the browser stack without its override (a checkout build where the published
// image was meant): every flag counts, in any order.
func TestParseArgsKeepsEveryFlagInAnyOrder(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		want    options
		command []string
	}{
		{[]string{"--browser", "--quiet", "--compose", "a.yaml", "--compose", "b.yaml", "--", "go", "test"},
			options{keycloak: true, browser: true, quiet: true, extras: []string{"a.yaml", "b.yaml"}}, []string{"go", "test"}},
		{[]string{"--compose", "a.yaml", "--keycloak", "--", "run.sh"},
			options{keycloak: true, extras: []string{"a.yaml"}}, []string{"run.sh"}},
	} {
		opts, command, err := parseArgs(tc.args)
		if err != nil || !reflect.DeepEqual(opts, tc.want) || !slices.Equal(command, tc.command) {
			t.Errorf("parseArgs(%q) = %+v, %q, %v; want %+v, %q", tc.args, opts, command, err, tc.want, tc.command)
		}
	}
	for _, args := range [][]string{{"--browser"}, {"--compose"}, {"--quiet", "--"}, {"--unknown", "--", "x"}} {
		if _, _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%q) = no error, want the usage", args)
		}
	}
}
