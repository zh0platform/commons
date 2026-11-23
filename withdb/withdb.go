// Command withdb runs a test command against a fresh PostgreSQL 18.7 server of its own: `go run ./test/withdb -- go test
// ...`. Each run is one Compose project named for its process, removed with its volumes and built images on every
// exit; a run killed before its cleanup is named by the next run's start. It exits 0 only when the command passed and
// the project is gone.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.kloudbox.de/commons/bff/internal/config"
	"git.kloudbox.de/commons/bff/test/browser"
	"git.kloudbox.de/commons/bff/test/keycloak"
	"git.kloudbox.de/commons/bff/test/testdb"
)

const (
	composeFile  = "test/postgres.yaml"
	keycloakFile = "test/keycloak/keycloak.yaml"
	browserFile  = "test/browser/browser.yaml"
	// projectPrefix names every project withdb starts. After it come this machine's tag and the process ID of the run
	// that owns the project: the record from which a later start tells an orphan from another session's live run.
	projectPrefix = "bff-test-"
	startTimeout  = "240" // a cold Keycloak takes about a minute
)

// hba allows TLS for every login, and plaintext only for bff's logins on the lost-commit tests' databases, which the
// test forwarder reaches on loopback. The image's own health check uses the server's socket.
const hba = `local all postgres trust
hostssl all all 0.0.0.0/0 scram-sha-256
hostssl all all ::/0 scram-sha-256
hostnossl "/^` + testdb.PlaintextPrefix + `" bff_runtime,bff_migrate,bff_lifecycle 0.0.0.0/0 scram-sha-256
`

func main() {
	opts, command, err := parseArgs(os.Args[1:])
	if err != nil {
		fail(err.Error())
	}
	os.Exit(run(command, opts))
}

// options are withdb's flags.
type options struct {
	keycloak, browser bool     // the browser stack brings Keycloak
	quiet             bool     // a failed run prints no service logs (a CI job's shared log, as make capacity's)
	extras            []string // --compose <file>: Compose files of the command's own, after the built-in ones
}

const usage = "usage: go run ./test/withdb [--keycloak] [--browser] [--quiet] [--compose <file>]... -- <command> [args...]"

// parseArgs reads withdb's flags, in any order, up to `--`; what follows is the command.
func parseArgs(args []string) (options, []string, error) {
	var opts options
	for len(args) > 0 {
		switch args[0] {
		case "--keycloak":
			opts.keycloak = true
		case "--browser":
			opts.browser, opts.keycloak = true, true
		case "--quiet":
			opts.quiet = true
		case "--compose":
			if len(args) < 2 {
				return options{}, nil, errors.New(usage)
			}
			opts.extras = append(opts.extras, args[1])
			args = args[1:]
		case "--":
			if len(args) == 1 {
				return options{}, nil, errors.New(usage)
			}
			return opts, args[1:], nil
		default:
			return options{}, nil, errors.New(usage)
		}
		args = args[1:]
	}
	return options{}, nil, errors.New(usage)
}

func fail(message string) {
	_, _ = fmt.Fprintln(os.Stderr, "withdb:", message)
	os.Exit(2)
}

// run runs command against PostgreSQL and, with opts.keycloak, a Keycloak of the same project; opts.browser adds the
// browser stack, opts.extras Compose files of the command's own.
func run(command []string, opts options) int {
	root, err := os.Getwd()
	if err != nil {
		fail(err.Error())
	}
	reportOrphans()
	// withdb catches the stop signals instead of dying of them, so its cleanup always runs: one that arrives during
	// setup skips the command, one during the command is passed on to it. (An ignored signal would be inherited by
	// the command, which could then not be stopped.)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	p := project{root: root, name: owner() + strconv.Itoa(os.Getpid()) + "-" + strings.ToLower(rand.Text()[:8])}
	code := 1
	settings, err := startServer(p, opts)
	switch {
	case err != nil:
		_, _ = fmt.Fprintln(os.Stderr, "withdb: setup failed:", err)
	case len(stop) > 0:
		_, _ = fmt.Fprintln(os.Stderr, "withdb: stopped during setup")
	default:
		code = runCommand(command, settings, stop)
	}
	// A quiet run's output is shared (a CI job's log for make capacity, the owner's terminal for make protected): the
	// services' logs, which can hold the provisioning's statements, stay out of it.
	if code != 0 && !opts.quiet {
		logs, _ := p.compose("logs", "--no-color")
		_, _ = os.Stderr.Write(logs)
	}
	if _, err := p.compose("down", "--volumes", "--remove-orphans", "--rmi", "local"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "withdb: removing the project failed:", err)
		return max(code, 1)
	}
	return code
}

// project is one run's Compose project; env holds the values its Compose files read.
type project struct {
	root, name string
	env        []string
}

// compose runs one Compose command on the project. Only `up` reads the Compose file (with the run's secrets); the
// others address the project by its name.
func (p project) compose(args ...string) ([]byte, error) {
	cmd := exec.Command("docker", append([]string{"compose", "--project-name", p.name}, args...)...)
	cmd.Dir, cmd.Env, cmd.Stderr = p.root, append(childEnviron(), p.env...), os.Stderr
	return cmd.Output()
}

// owner is the start of this machine's project names: the prefix and a tag of its host name and, on Linux, its process
// namespace, so a Docker daemon shared with another machine or container never makes process IDs this one cannot see
// look dead here.
func owner() string {
	host, _ := os.Hostname()
	namespace, _ := os.Readlink("/proc/self/ns/pid") // empty where there is none, as on macOS
	tag := sha256.Sum256([]byte(host + "\x00" + namespace))
	return projectPrefix + hex.EncodeToString(tag[:4]) + "-"
}

// published is the loopback address Docker chose for a service's port.
func (p project) published(service, port string) (string, string, error) {
	answer, err := p.compose("port", service, port)
	if err != nil {
		return "", "", fmt.Errorf("compose port %s: %w", service, err)
	}
	host, published, err := net.SplitHostPort(strings.TrimSpace(string(answer)))
	if err != nil {
		return "", "", fmt.Errorf("compose port %s answered %q", service, answer)
	}
	return host, published, nil
}

// reportOrphans names the projects of withdb runs on this machine whose process is gone (a run killed before its
// cleanup), with the command that removes them. It removes nothing: a name is no proof of ownership (docker.md).
func reportOrphans() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // it answers in well under a second
	defer cancel()
	listed, err := exec.CommandContext(ctx, "docker", "compose", "ls", "--all", "--format", "json").Output()
	if err != nil {
		return // advisory only: the run's own Compose calls meet a Docker that does not answer
	}
	var projects []struct{ Name string }
	_ = json.Unmarshal(listed, &projects)
	for _, listedProject := range projects {
		rest, ours := strings.CutPrefix(listedProject.Name, owner())
		pid, _, _ := strings.Cut(rest, "-")
		number, err := strconv.Atoi(pid)
		if ours && err == nil && !processRuns(number) {
			_, _ = fmt.Fprintf(os.Stderr, "withdb: %s is left from a run that ended without its cleanup; remove it with: docker compose -p %s down --volumes --remove-orphans --rmi local\n",
				listedProject.Name, listedProject.Name)
		}
	}
}

// processRuns says whether a process ID is in use on this machine. A process of another user answers EPERM and runs.
func processRuns(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// startServer starts the database and creates what infra provisions: the three logins and schema bff.
func startServer(p project, opts options) (map[string]string, error) {
	authority, err := testdb.NewAuthority()
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := authority.ServerCertificate()
	if err != nil {
		return nil, err
	}
	spki, err := publicKeyPin(certPEM)
	if err != nil {
		return nil, err
	}
	adminPassword, loginPassword := rand.Text(), rand.Text()
	// Compose reads the servers' secrets from its own environment: nothing of them is written to a file a killed run
	// could leave behind. The test command gets them too, for its own Compose commands (the browser stack).
	values := map[string]string{
		"BFF_TEST_ADMIN_PASSWORD": adminPassword, "BFF_TEST_SERVER_CERT": certPEM,
		"BFF_TEST_SERVER_KEY": keyPEM, "BFF_TEST_PG_HBA": hba,
		// Chromium trusts exactly this run's server key, so every other site keeps its normal certificate checks.
		"BFF_TEST_SERVER_SPKI": spki,
	}
	if opts.keycloak {
		values["KEYCLOAK_REALMS"] = filepath.Join(p.root, "test/keycloak/realms")
	}
	for name, value := range values {
		p.env = append(p.env, name+"="+value)
	}
	paths := []string{composeFile}
	if opts.keycloak {
		paths = append(paths, keycloakFile)
	}
	if opts.browser {
		paths = append(paths, browserFile)
	}
	paths = append(paths, opts.extras...)
	var files []string
	for _, path := range paths {
		files = append(files, "--file", path)
	}
	if _, err := p.compose(append(files, "up", "--detach", "--wait", "--wait-timeout", startTimeout)...); err != nil {
		return nil, fmt.Errorf("compose up: %w", err)
	}
	host, port, err := p.published("postgres", "5432")
	if err != nil {
		return nil, err
	}
	settings := map[string]string{
		testdb.HostVariable: host, testdb.PortVariable: port, testdb.CAVariable: authority.PEM(),
		testdb.AdminVariable: adminPassword, testdb.LoginsVariable: loginPassword,
	}
	if opts.keycloak {
		host, port, err := p.published("keycloak", "8443")
		if err != nil {
			return nil, err
		}
		settings[keycloak.URLVariable], settings[keycloak.CAVariable] = "https://"+net.JoinHostPort(host, port), authority.PEM()
	}
	if opts.browser {
		host, port, err := p.published("chrome", "9222")
		if err != nil {
			return nil, err
		}
		// The test starts the bff services itself, on databases it migrated: it gets the Compose command line.
		command, _ := json.Marshal(append([]string{"--project-name", p.name}, files...))
		settings[browser.DevToolsVariable], settings[browser.ComposeVariable] = net.JoinHostPort(host, port), string(command)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := testdb.Provision(ctx, settings); err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}
	if opts.browser || len(opts.extras) > 0 {
		maps.Copy(settings, values)
	}
	if len(opts.extras) > 0 {
		// The command addresses the stack with plain docker compose, over the same files in the same order.
		settings["COMPOSE_PROJECT_NAME"], settings["COMPOSE_FILE"] = p.name, strings.Join(paths, string(os.PathListSeparator))
	}
	return settings, nil
}

// publicKeyPin is the base64 SHA-256 of a PEM certificate's public key, the form Chromium's
// --ignore-certificate-errors-spki-list takes.
func publicKeyPin(certPEM string) (string, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return "", errors.New("the server certificate is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// runCommand runs the test command, passing on every stop signal withdb receives meanwhile.
func runCommand(command []string, settings map[string]string, stop <-chan os.Signal) int {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = commandEnviron()
	for name, value := range settings {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "withdb:", err)
		return 1
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case received := <-stop:
				_ = cmd.Process.Signal(received)
			case <-done:
				return
			}
		}
	}()
	if err := cmd.Wait(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return max(exit.ExitCode(), 1)
		}
		_, _ = fmt.Fprintln(os.Stderr, "withdb:", err)
		return 1
	}
	return 0
}

// commandEnviron is the test command's inherited environment: childEnviron without the addresses withdb hands a
// command, which it sets for this run only. An inherited one (from a shell that ran or exported another run's) would
// point a run without that service at the other run's server instead of failing.
func commandEnviron() []string {
	runnerVariables := []string{ // every address startServer can set
		testdb.HostVariable, testdb.PortVariable, testdb.CAVariable, testdb.AdminVariable, testdb.LoginsVariable,
		keycloak.URLVariable, keycloak.CAVariable, browser.DevToolsVariable, browser.ComposeVariable,
		"COMPOSE_PROJECT_NAME", "COMPOSE_FILE",
	}
	return slices.DeleteFunc(childEnviron(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains(runnerVariables, name)
	})
}

// childEnviron is this process's environment without the variables bff refuses at startup.
func childEnviron() []string {
	var kept []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !config.IsAmbient(name) {
			kept = append(kept, entry)
		}
	}
	return kept
}
