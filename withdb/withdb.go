// Package withdb starts one run's test stack, a PostgreSQL server of its own and, if the product asks, a Keycloak, as
// a Compose project, and runs a command against it. A product's runner program is a few lines around it:
//
//	stack, err := withdb.Start(product) // exit 1 on an error
//	status, err := stack.Run(command)   // after any further step, such as Published
//	err = stack.Close()                 // on every path once Start answered a stack
//
// The runner exits 2 for its own usage error, 1 when Start or a step before the command fails or the command could
// not run, otherwise with the command's status, raised to at least 1 when Close fails. Each run removes its project and
// its folder on every exit it makes itself; a run killed hard is named, with the commands that remove it, by the next
// start of the same product on the same machine. A run never removes what another run started. One stack per process.
//
// With P the product's Variables, Compose and the product's own Compose files read P+ADMIN_PASSWORD, P+SERVER_CERT,
// P+SERVER_KEY, P+SERVER_SPKI (the server key's pin, as Chromium takes it), P+PG_HBA, P+ROOT (the product's Root) and,
// with Keycloak, KEYCLOAK_REALMS. The command gets P+DB_HOST, P+DB_PORT, P+DB_CA, P+DB_ADMIN_PASSWORD and
// P+DB_LOGIN_PASSWORD; with Keycloak, KEYCLOAK_TEST_URL and KEYCLOAK_TEST_CA; when the product gives Compose files, also
// Compose's values, COMPOSE_PROJECT_NAME and COMPOSE_FILE. A product's own Compose file may give the postgres service's
// environment entry P+PG_FLAGS, which replaces the module's server flags (-c log_connections=receipt,setup_durations),
// the last -c of a name winning. The server certificate names 127.0.0.1, ::1, host.docker.internal, postgres and
// idp.test, the product's Names, and never localhost. Every process the runner starts gets the inherited
// environment without the variables the product refuses, without every COMPOSE_* variable and without every name
// above, so a shell that ran or exported another run's settings cannot point this run at that run's servers.
package withdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"embed"
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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"git.kloudbox.de/commons/commons/keycloak"
	"git.kloudbox.de/commons/commons/testdb"
)

// The services and container ports of the stack.
const (
	postgresService = "postgres"
	postgresPort    = "5432"
	keycloakService = "keycloak"
	keycloakPort    = "8443"
)

const (
	// placeholder starts every variable the embedded Compose files read; the product's Variables replace it.
	placeholder = "WITHDB_"
	// startBound covers bringing the services up on a cold machine. Measured 2026-11-23: downloading the arm64 images
	// takes 35 s (Keycloak, 264 MB) and 28 s (PostgreSQL, 167 MB) one layer after another, a cold Keycloak start about
	// a minute; with extraction about 3 minutes, so 8 leaves more than twice that. Warm: about 3 s, 36 s with Keycloak.
	startBound = 8 * time.Minute
	// healthWait is Compose's own wait for the services' health, inside startBound: a cold Keycloak takes about a
	// minute to be healthy (36 s warm on the CI host), so 4 minutes.
	healthWait = "240"
	// provisionBound covers creating the logins, the template and the schema: under a second measured, a minute for a
	// loaded host.
	provisionBound = time.Minute
	// removeBound is the fresh bound of the removal, whatever ended the run: removing a project measured 2 to 3 s,
	// with the browser stack's built images under 10 s; 2 minutes for a loaded daemon.
	removeBound = 2 * time.Minute
	// callBound covers a Compose call that only reads (port, ls, logs): measured well under a second.
	callBound = 30 * time.Second
)

//go:embed postgres.yaml keycloak.yaml
var embedded embed.FS

// Stack is one run's started Compose project.
type Stack struct {
	product    testdb.Product
	name       string
	folder     string
	files      []string // absolute, in order
	base       []string // the inherited environment every process the runner starts gets
	composeEnv []string // the run's values for Compose and the product's files
	commandEnv []string // the run's values for the command

	signals chan os.Signal
	stop    context.Context    // ended by a signal that came before the command
	stopNow context.CancelFunc // ends stop, and with it the Compose call in progress
	mu      sync.Mutex
	command *os.Process // the running command, which receives every signal
	passed  bool        // the command ended with status 0
}

// errStopped answers a run that a signal stopped before its command.
var errStopped = errors.New("stopped during setup")

// Start checks the product's description, reports what killed runs of the product left, and brings up the run's
// stack: its folder, authority, server certificate and passwords, the Compose files, the services, healthy, and the
// provisioned server. It catches INT and TERM from then on. When a step fails or a signal arrives it prints the
// services' logs (unless the product is quiet), removes what it started and answers the error; nothing is left to
// close.
func Start(p testdb.Product) (*Stack, error) {
	if err := p.Check(); err != nil {
		return nil, fmt.Errorf("setup failed: checking the description: %w", err)
	}
	s := &Stack{product: p, name: owner(p) + strconv.Itoa(os.Getpid()) + "-" + strings.ToLower(rand.Text()[:8])}
	s.folder = filepath.Join(os.TempDir(), s.name)
	s.base = inherited(p)
	s.stop, s.stopNow = context.WithCancel(context.Background())
	s.signals = make(chan os.Signal, 1)
	signal.Notify(s.signals, os.Interrupt, syscall.SIGTERM)
	go s.watch()
	reportLeftovers(s.stop, p, s.base)
	// The folder is this run's only once it made it: a failure here leaves nothing to remove, and nothing is removed.
	if err := os.Mkdir(s.folder, 0o700); err != nil {
		s.stopSignals()
		return nil, fmt.Errorf("setup failed: %w", err)
	}
	if err := s.start(); err != nil {
		if removeErr := s.remove(!p.Quiet); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
		s.stopSignals()
		return nil, err
	}
	return s, nil
}

func (s *Stack) start() error {
	server, err := s.writeFiles()
	if err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}
	ctx, cancel := context.WithTimeout(s.stop, startBound)
	defer cancel()
	up := []string{}
	for _, file := range s.files {
		up = append(up, "--file", file)
	}
	up = append(up, "up", "--detach", "--wait", "--wait-timeout", healthWait)
	if _, err := s.compose(ctx, up...); err != nil {
		return s.setupError("bringing up", startBound, err)
	}
	host, port, err := s.Published(postgresService, postgresPort)
	if err != nil {
		return s.setupError("reading a port", callBound, err)
	}
	server.Host, server.Port = host, port
	// A bound of its own, not what is left of the start's, so an expired one is named truthfully.
	provisionCtx, cancelProvision := context.WithTimeout(s.stop, provisionBound)
	defer cancelProvision()
	if err := testdb.Provision(provisionCtx, s.product, server); err != nil {
		return s.setupError("provisioning", provisionBound, err)
	}
	s.commandEnv = s.product.ServerEnviron(server)
	if s.product.Realms != "" {
		host, port, err := s.Published(keycloakService, keycloakPort)
		if err != nil {
			return s.setupError("reading a port", callBound, err)
		}
		s.commandEnv = append(s.commandEnv,
			keycloak.URLVariable+"=https://"+net.JoinHostPort(host, port), keycloak.CAVariable+"="+server.CA)
	}
	if len(s.product.Files) > 0 {
		// The command addresses the stack with plain docker compose, over the same files in the same order.
		s.commandEnv = append(s.commandEnv, s.composeEnv...)
		s.commandEnv = append(s.commandEnv,
			"COMPOSE_PROJECT_NAME="+s.name, "COMPOSE_FILE="+strings.Join(s.files, string(os.PathListSeparator)))
	}
	if s.stop.Err() != nil {
		return errStopped
	}
	return nil
}

// setupError names the failed step, or the signal or the step's bound that ended it.
func (s *Stack) setupError(step string, bound time.Duration, err error) error {
	switch {
	case s.stop.Err() != nil:
		return errStopped
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("setup failed: %s: no answer within %v: %w", step, bound, err)
	}
	return fmt.Errorf("setup failed: %s: %w", step, err)
}

// writeFiles makes the run's secrets and writes the module's Compose files, with the product's prefix, into the run's
// folder. The secrets reach Compose only through its environment, never a file. It answers the server's authority and
// passwords; its address comes once it runs.
func (s *Stack) writeFiles() (testdb.Server, error) {
	p := s.product
	authority, err := testdb.NewAuthority()
	if err != nil {
		return testdb.Server{}, err
	}
	certPEM, keyPEM, err := authority.ServerCertificate(append([]string{postgresService, keycloakAlias}, p.Names...)...)
	if err != nil {
		return testdb.Server{}, err
	}
	pin, err := publicKeyPin(certPEM)
	if err != nil {
		return testdb.Server{}, err
	}
	server := testdb.Server{CA: authority.PEM(), AdminPassword: rand.Text(), LoginPassword: rand.Text()}
	for _, value := range []struct{ suffix, value string }{
		{adminPassword, server.AdminPassword}, {serverCert, certPEM}, {serverKey, keyPEM},
		{serverPin, pin}, {pgHBA, hba(p)}, {root, p.Root},
	} {
		s.composeEnv = append(s.composeEnv, p.Variables+value.suffix+"="+value.value)
	}
	names := []string{"postgres.yaml"}
	if p.Realms != "" {
		names = append(names, "keycloak.yaml")
		s.composeEnv = append(s.composeEnv, realmsVariable+"="+p.Realms)
	}
	for _, name := range names {
		content, err := embedded.ReadFile(name)
		if err != nil {
			return testdb.Server{}, err
		}
		path := filepath.Join(s.folder, name)
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(content), placeholder, p.Variables)), 0o600); err != nil {
			return testdb.Server{}, err
		}
		s.files = append(s.files, path)
	}
	for _, file := range p.Files {
		if !filepath.IsAbs(file) {
			file = filepath.Join(p.Root, file)
		}
		s.files = append(s.files, file)
	}
	return server, nil
}

// The variables, after the product's prefix, that Compose and the product's files read; and the realm folder's.
const (
	adminPassword  = "ADMIN_PASSWORD"
	serverCert     = "SERVER_CERT"
	serverKey      = "SERVER_KEY"
	serverPin      = "SERVER_SPKI"
	pgHBA          = "PG_HBA"
	root           = "ROOT"
	realmsVariable = "KEYCLOAK_REALMS"
	// keycloakAlias is Keycloak's name on the stack's network (keycloak.yaml), on the server certificate.
	keycloakAlias = "idp.test"
)

// hba allows TLS for every login, and plaintext only for the product's logins on the lost-commit tests' databases,
// which the relay reaches on loopback. The image's own health check uses the server's socket.
func hba(p testdb.Product) string {
	return "local all " + testdb.Administrator + " trust\n" +
		"hostssl all all 0.0.0.0/0 scram-sha-256\n" +
		"hostssl all all ::/0 scram-sha-256\n" +
		`hostnossl "/^` + testdb.PlaintextPrefix + `" ` + strings.Join(p.Logins, ",") + " 0.0.0.0/0 scram-sha-256\n"
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

// Published is the loopback host and port Docker chose for a service's container port.
func (s *Stack) Published(service, port string) (string, string, error) {
	ctx, cancel := context.WithTimeout(s.stop, callBound)
	defer cancel()
	answer, err := s.compose(ctx, "port", service, port)
	if err != nil {
		return "", "", fmt.Errorf("compose port %s: %w", service, err)
	}
	host, published, err := net.SplitHostPort(strings.TrimSpace(string(answer)))
	if err != nil {
		return "", "", fmt.Errorf("compose port %s answered %q", service, answer)
	}
	return host, published, nil
}

// Run runs the command with the given NAME=value variables and the run's settings, which win over them and over every
// inherited variable, and passes every INT and TERM the runner receives on to it. The command's lifetime has no bound
// of its own: the caller's (a CI job's timeout) applies. It answers the command's status, at least 1 when it did not
// end with 0; a command skipped by a signal that came first, or one that could not start, is an error with status 1.
func (s *Stack) Run(command []string, variables ...string) (int, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = slices.Concat(s.base, variables, s.commandEnv)
	s.mu.Lock()
	if s.stop.Err() != nil {
		s.mu.Unlock()
		return 1, errStopped
	}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return 1, err
	}
	s.command = cmd.Process
	s.mu.Unlock()
	err := cmd.Wait()
	s.mu.Lock()
	s.command = nil
	s.mu.Unlock()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return max(exit.ExitCode(), 1), nil
	}
	if err != nil {
		return 1, err
	}
	s.passed = true
	return 0, nil
}

// Close prints the services' logs unless a command passed or the product is quiet, removes the project with its
// volumes, orphans and locally built images, and the run's folder, under a fresh bound, and stops catching signals.
// It answers an error naming the project and the commands that remove it when the removal fails.
func (s *Stack) Close() error {
	err := s.remove(!s.passed && !s.product.Quiet)
	s.stopSignals()
	return err
}

// remove prints the logs if asked and removes the project and the folder.
func (s *Stack) remove(withLogs bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), removeBound)
	defer cancel()
	if withLogs {
		logsCtx, cancelLogs := context.WithTimeout(ctx, callBound)
		logs, _ := s.compose(logsCtx, "logs", "--no-color")
		cancelLogs()
		_, _ = os.Stderr.Write(logs)
	}
	// The folder goes only once the project is gone: it is what the next start finds a leftover by.
	if _, err := s.compose(ctx, downArgs...); err != nil {
		return fmt.Errorf("removing %s failed: %w; remove it with: %s", s.name, err, removal(s.name, s.folder))
	}
	if err := os.RemoveAll(s.folder); err != nil {
		return fmt.Errorf("removing the folder of %s failed: %w; remove it with: rm -rf %s", s.name, err, quoted(s.folder))
	}
	return nil
}

// removal is the commands that remove a run's project, which need no Compose file, and its folder by hand.
func removal(name, folder string) string {
	return projectRemoval(name) + " && rm -rf " + quoted(folder)
}

// projectRemoval is the command that removes a run's project by its name.
func projectRemoval(name string) string {
	return "docker compose -p " + name + " " + strings.Join(downArgs, " ")
}

// downArgs remove a project with its volumes, orphans and the images Compose built for it.
var downArgs = []string{"down", "--volumes", "--remove-orphans", "--rmi", "local"}

// quoted is a path as one shell word.
func quoted(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// compose runs one Compose command on the project, from the product's root, in the run's Compose environment.
func (s *Stack) compose(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "--project-name", s.name}, args...)...)
	cmd.Dir, cmd.Env, cmd.Stderr = s.product.Root, slices.Concat(s.base, s.composeEnv), os.Stderr
	output, err := cmd.Output()
	if err != nil && ctx.Err() != nil {
		// A process the context killed reports only "signal: killed": the context says why.
		err = fmt.Errorf("%w (%w)", err, ctx.Err())
	}
	return output, err
}

// watch passes every INT and TERM to the running command; before the command, it marks the run stopped and ends the
// Compose call in progress.
func (s *Stack) watch() {
	for received := range s.signals {
		s.mu.Lock()
		if s.command != nil {
			_ = s.command.Signal(received)
		} else {
			s.stopNow()
		}
		s.mu.Unlock()
	}
}

func (s *Stack) stopSignals() {
	signal.Stop(s.signals)
	close(s.signals)
	s.stopNow()
}

// inherited is the runner's environment without the variables the product refuses, without Compose's own and
// without every name the runner sets, so a shell that ran or exported another run's settings cannot point this run
// at that run's servers.
func inherited(p testdb.Product) []string {
	runnerNames := slices.Concat(p.ServerVariables(), []string{keycloak.URLVariable, keycloak.CAVariable, realmsVariable})
	for _, suffix := range []string{adminPassword, serverCert, serverKey, serverPin, pgHBA, root} {
		runnerNames = append(runnerNames, p.Variables+suffix)
	}
	var kept []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		refused := p.Refused != nil && p.Refused(name)
		if !refused && !strings.HasPrefix(name, "COMPOSE_") && !slices.Contains(runnerNames, name) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// owner is the start of this product's project names on this machine: the product's prefix and a tag of the host
// name and, on Linux, the process namespace, so a Docker daemon shared with another machine or container never makes
// process IDs this one cannot see look dead here.
func owner(p testdb.Product) string {
	host, _ := os.Hostname()
	namespace, _ := os.Readlink("/proc/self/ns/pid") // empty where there is none, as on macOS
	tag := sha256.Sum256([]byte(host + "\x00" + namespace))
	return p.Project + hex.EncodeToString(tag[:4]) + "-"
}

// reportLeftovers names the projects and folders of this product's runs on this machine whose process is gone (a run
// killed before its cleanup), with the commands that remove them. It removes nothing: a name is no proof of ownership.
// A failed listing is not fatal: the run's own Compose calls meet a Docker that does not answer. It sees only the
// projects of the Docker daemon it reaches and the folders under this process's temporary folder;
// Docker objects made before a run's first container are not listed by Compose.
func reportLeftovers(stop context.Context, p testdb.Product, environ []string) {
	prefix := owner(p)
	projects, folders := map[string]bool{}, map[string]bool{}
	ctx, cancel := context.WithTimeout(stop, callBound)
	defer cancel()
	list := exec.CommandContext(ctx, "docker", "compose", "ls", "--all", "--format", "json")
	list.Env = environ
	if listed, err := list.Output(); err == nil {
		var found []struct{ Name string }
		_ = json.Unmarshal(listed, &found)
		for _, project := range found {
			projects[project.Name] = isLeftover(prefix, project.Name)
		}
	}
	if entries, err := os.ReadDir(os.TempDir()); err == nil {
		for _, entry := range entries {
			folders[entry.Name()] = entry.IsDir() && isLeftover(prefix, entry.Name())
		}
	}
	names := map[string]bool{}
	maps.Copy(names, projects)
	maps.Copy(names, folders)
	for _, name := range slices.Sorted(maps.Keys(names)) {
		folder := filepath.Join(os.TempDir(), name)
		var subject string
		var commands []string
		if projects[name] {
			subject, commands = name, append(commands, projectRemoval(name))
		}
		if folders[name] {
			commands = append(commands, "rm -rf "+quoted(folder))
			if subject == "" {
				subject = "the folder " + folder
			}
		}
		if subject != "" {
			_, _ = fmt.Fprintf(os.Stderr, "withdb: %s is left from a run that ended without its cleanup; remove it with: %s\n",
				subject, strings.Join(commands, " && "))
		}
	}
}

// runSuffix is what follows a run's owner prefix: its process ID and 8 random characters (lowercased base32).
var runSuffix = regexp.MustCompile(`^([0-9]+)-[a-z2-7]{8}$`)

// isLeftover says whether a name is one of this machine's runs of the product whose process is gone. A name is no
// proof of who made it; the report only names what matches.
func isLeftover(prefix, name string) bool {
	rest, ours := strings.CutPrefix(name, prefix)
	found := runSuffix.FindStringSubmatch(rest)
	if !ours || found == nil {
		return false
	}
	pid, err := strconv.Atoi(found[1])
	return err == nil && !processRuns(pid)
}

// processRuns says whether a process ID is in use on this machine. A process of another user answers EPERM and runs.
func processRuns(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
