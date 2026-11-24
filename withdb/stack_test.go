//go:build stack

package withdb_test

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zh0platform/commons/internal/selftest"
)

// runBound is how long a run may take to print a line or to end: above the runner's own start bound (8 minutes,
// cold images), provisioning's minute and the removal's two, so a slow start fails with the runner's bound named.
const runBound = 12 * time.Minute

// prefix starts the names of the test product's projects and folders.
var prefix = selftest.Product("/").Project

// The runner as a real process, killed and stopped at each boundary it promises cleanup for, while a second run of
// its own keeps running beside it: each case leaves neither its Compose project nor its folder, and the second run is
// untouched. A hard kill leaves both, and the next start names them with the commands that remove them.
func TestRunnerRemovesWhatItStartedAndNothingElse(t *testing.T) {
	runner := buildRunner(t)
	temp := t.TempDir() // every run's folder lives here, so the test sees exactly its own
	// It outlives every case (runBound each), and ends only by the test's signal.
	beside := start(t, runner, temp, nil, "--", "sh", "-c", "echo ready; exec sleep 3600")
	beside.waitFor(t, "ready")

	t.Run("each boundary", func(t *testing.T) { // returns once its parallel cases ended
		cases(t, runner, temp)
	})
	if !listed(t, beside.project(t)) {
		t.Errorf("the run beside, %s, is gone", beside.project(t))
	}
	beside.signal(t, syscall.SIGTERM)
	_ = beside.wait(t)
	assertGone(t, beside)
}

// cases runs each boundary in a run of its own, in parallel: each has its own process ID, project and folder.
func cases(t *testing.T, runner, temp string) {
	statementLog := filepath.Join(t.TempDir(), "statements.yaml")
	if err := os.WriteFile(statementLog, []byte("services:\n  postgres:\n    environment:\n      SHOPTEST_PG_FLAGS: -c log_statement=all -c log_min_duration_statement=0 -c log_transaction_sample_rate=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A product file that gives Chromium a flag with spaces and builds an image of its own, from the image the module
	// already pulled, so the removal of built images is seen.
	browserFile := filepath.Join(t.TempDir(), "browser.yaml")
	if err := os.WriteFile(browserFile, []byte("services:\n  chrome:\n    command: [\"--user-data-dir=/tmp/withdb flag probe\"]\n"+
		"  built:\n    build:\n      context: .\n      dockerfile_inline: \"FROM "+postgresImage(t)+"\"\n"+
		"    entrypoint: [\"sleep\", \"infinity\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(t.TempDir(), "secrets")
	for _, tc := range []struct {
		name   string
		args   []string
		signal func(*testing.T, *run) // sent while the run is going; nil lets it end by itself
		status int
		output []string               // what standard error must hold
		check  func(*testing.T, *run) // after the run ended
	}{
		{name: "command ends", args: []string{"--", "sh", "-c", "exit 3"}, status: 3},
		{name: "command cannot start", args: []string{"--", "/nonexistent/command"}, status: 1,
			output: []string{"no such file"}},
		{name: "interrupt during start", args: []string{"--", "true"}, status: 1,
			signal: func(t *testing.T, r *run) { r.waitForFolder(t); r.signal(t, syscall.SIGINT) },
			output: []string{"stopped during setup"}},
		{name: "terminate during start", args: []string{"--", "true"}, status: 1,
			signal: func(t *testing.T, r *run) { r.waitForFolder(t); r.signal(t, syscall.SIGTERM) },
			output: []string{"stopped during setup"}},
		{name: "terminate during the command", args: []string{"--", "sh", "-c", "echo ready; exec sleep 600"}, status: 1,
			signal: func(t *testing.T, r *run) { r.waitFor(t, "ready"); r.signal(t, syscall.SIGTERM) }},
		{name: "interrupt during the command", args: []string{"--", "sh", "-c", "echo ready; exec sleep 600"}, status: 1,
			signal: func(t *testing.T, r *run) { r.waitFor(t, "ready"); r.signal(t, syscall.SIGINT) }},
		// A browser without any product file: the command gets DevTools' address and no Compose variable, and DevTools
		// answers through that address at once.
		{name: "browser without product files", status: 0, args: []string{"--browser", "--", "sh", "-c",
			`test -z "$COMPOSE_FILE" && test -z "$SHOPTEST_ADMIN_PASSWORD" && curl -fsS --max-time 10 "http://$SHOPTEST_DEVTOOLS/json/version" | grep -q '"Browser"'`}},
		// The command records the run's passwords and finds neither them nor a private key in the run's folder (status 3;
		// 1 when it finds one, or no folder); its failure prints the services' logs, which must not hold them either.
		{name: "failed command, secrets kept out", status: 3,
			args: []string{"--", "sh", "-c", `printf '%s\n%s\n' "$SHOPTEST_DB_ADMIN_PASSWORD" "$SHOPTEST_DB_LOGIN_PASSWORD" > ` + secrets +
				` && set -- "$TMPDIR"/` + prefix + `*-$PPID-* && test -d "$1"` +
				` && ! grep -rqF -e "$SHOPTEST_DB_ADMIN_PASSWORD" -e "$SHOPTEST_DB_LOGIN_PASSWORD" -e "PRIVATE KEY" "$1" && exit 3`},
			check: func(t *testing.T, r *run) {
				recorded, err := os.ReadFile(secrets)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range append(strings.Fields(string(recorded)), "PRIVATE KEY") {
					if strings.Contains(r.output(), secret) {
						t.Errorf("standard error holds a password or a private key (output withheld)")
					}
				}
			}},
		// A product file's Chromium flag reaches the running browser as one argument, spaces included.
		{name: "browser with a product file", status: 0, args: []string{"--browser", "--compose", browserFile, "--", "sh", "-c",
			`docker compose exec -T chrome cat /proc/1/cmdline | tr '\0' '\n' | grep -qxF -- '--user-data-dir=/tmp/withdb flag probe'`}},
		{name: "failed start, statements logged", status: 1,
			args: []string{"--compose", statementLog, "--login", "shop_migrate", "--", "true"},
			output: []string{"setup failed: provisioning", `role "shop_migrate" already exists`,
				"database system is ready to accept connections"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := start(t, runner, temp, nil, tc.args...)
			if tc.signal != nil {
				tc.signal(t, r)
			}
			status := r.wait(t)
			// A failure prints the run's output, except when it holds a password: then nothing of it.
			out := r.output()
			if strings.Contains(out, "PASSWORD '") {
				t.Errorf("output holds a password-carrying statement (output withheld)")
				out = "(withheld)"
			}
			if status != tc.status {
				t.Errorf("exit status %d, want %d; output:\n%s", status, tc.status, out)
			}
			for _, want := range tc.output {
				if !strings.Contains(r.output(), want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if tc.check != nil {
				tc.check(t, r)
			}
			assertGone(t, r)
		})
	}

	// The command's real environment: an inherited setting of another run and a refused variable do not reach it; the
	// run's own settings and a setting the runner does not manage do.
	t.Run("the command's environment", func(t *testing.T) {
		t.Parallel()
		inherited := []string{"SHOP_TYPO=1", "SHOPTEST_DB_HOST=another-run", "SHOPTEST_DEVTOOLS=another-run", "SHOPTEST_PG_FLAGS=-c fsync=on"}
		r := start(t, runner, temp, inherited, "--", "env")
		status := r.wait(t)
		if status != 0 {
			t.Fatalf("exit %d:\n%s", status, r.output())
		}
		environment := strings.Join(r.stdout(t), "\n") + "\n"
		for _, absent := range []string{"SHOP_TYPO=", "SHOPTEST_DB_HOST=another-run", "SHOPTEST_DEVTOOLS="} {
			if strings.Contains(environment, absent) {
				t.Errorf("the command's environment holds %s", absent)
			}
		}
		for _, present := range []string{"SHOPTEST_PG_FLAGS=-c fsync=on\n", "SHOPTEST_DB_HOST=", "SHOPTEST_DB_CA=-----BEGIN"} {
			if !strings.Contains(environment, present) {
				t.Errorf("the command's environment lacks %s", present)
			}
		}
		assertGone(t, r)
	})

	// A removal that fails raises a passed command's status to 1 and keeps the folder, which names the project for the
	// next start.
	t.Run("failed removal", func(t *testing.T) {
		t.Parallel()
		r := start(t, runner, temp, []string{"PATH=" + failingDown(t) + string(os.PathListSeparator) + os.Getenv("PATH")}, "--", "true")
		status := r.wait(t)
		r.removeAfter(t)
		if status != 1 || r.folder() == "" || !strings.Contains(r.output(), "remove it with: docker compose -p "+r.project(t)) {
			t.Errorf("a failed removal: exit %d, folder %q, want 1, the folder kept and its removal named:\n%s", status, r.folder(), r.output())
		}
	})

	t.Run("hard kill", func(t *testing.T) {
		t.Parallel()
		killed := start(t, runner, temp, nil, "--", "sh", "-c", "echo ready; exec sleep 600")
		killed.waitFor(t, "ready")
		project, folder := killed.project(t), killed.folder()
		killed.removeAfter(t)
		killed.signal(t, syscall.SIGKILL)
		_ = killed.wait(t)
		next := start(t, runner, temp, nil, "--", "true")
		if status := next.wait(t); status != 0 {
			t.Fatalf("the next run: exit %d:\n%s", status, next.output())
		}
		want := project + " is left from a run that ended without its cleanup; remove it with: docker compose -p " + project
		if !strings.Contains(next.output(), want) || !strings.Contains(next.output(), "rm -rf '"+folder+"'") {
			t.Errorf("the next start does not name the killed run's project and folder:\n%s", next.output())
		}
		if _, err := os.Stat(folder); !listed(t, project) || err != nil {
			t.Errorf("the next start removed %s or its folder (%v), which it did not start", project, err)
		}
	})
}

// run is one runner process of the test.
type run struct {
	cmd    *exec.Cmd
	temp   string
	mu     sync.Mutex
	stderr bytes.Buffer
	out    []string // standard output's lines
	lines  chan string
	read   chan struct{} // standard output reached its end
	done   chan struct{}
	status int
}

func buildRunner(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "withdb")
	build := exec.Command("go", "build", "-o", binary, "./internal/selftest/withdb")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the runner: %v\n%s", err, output)
	}
	return binary
}

// start starts a runner with its folders under temp and the given inherited variables besides the test's own.
func start(t *testing.T, runner, temp string, inherited []string, args ...string) *run {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	r := &run{temp: temp, lines: make(chan string, 100), read: make(chan struct{}), done: make(chan struct{})}
	r.cmd = exec.Command(runner, args...)
	r.cmd.Dir = root
	r.cmd.Env = append(append(os.Environ(), inherited...), "TMPDIR="+temp)
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // the test ends every process the run left
	stdout, writer, err := os.Pipe()                        // a pipe of the test's own: Wait does not wait for its reader
	if err != nil {
		t.Fatal(err)
	}
	r.cmd.Stdout = writer
	r.cmd.Stderr = &lockedWriter{r: r}
	// A runner killed hard leaves its command running, which holds the output pipes: Wait stops waiting for them.
	r.cmd.WaitDelay = 2 * time.Second
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	t.Cleanup(func() {
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
		_ = stdout.Close()
	})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			r.mu.Lock()
			r.out = append(r.out, scanner.Text())
			r.mu.Unlock()
			select {
			case r.lines <- scanner.Text():
			default: // nobody waits for a line of a run that prints many
			}
		}
		close(r.read)
	}()
	go func() {
		_ = r.cmd.Wait()
		r.status = r.cmd.ProcessState.ExitCode()
		close(r.done)
	}()
	return r
}

type lockedWriter struct{ r *run }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	return w.r.stderr.Write(p)
}

// stdout is standard output's lines once it reached its end.
func (r *run) stdout(t *testing.T) []string {
	t.Helper()
	select {
	case <-r.read:
	case <-time.After(time.Minute):
		t.Fatal("standard output did not end within a minute")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.out)
}

func (r *run) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stderr.String()
}

func (r *run) waitFor(t *testing.T, line string) {
	t.Helper()
	deadline := time.After(runBound)
	for {
		select {
		case got := <-r.lines:
			if got == line {
				return
			}
		case <-r.done:
			t.Fatalf("the run ended before %q:\n%s", line, r.output())
		case <-deadline:
			t.Fatalf("no %q within %v:\n%s", line, runBound, r.output())
		}
	}
}

// waitForFolder waits until the run made its folder, which Start does before bringing the services up.
func (r *run) waitForFolder(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if r.folder() != "" {
			return
		}
		<-time.After(10 * time.Millisecond)
	}
	t.Fatalf("the run made no folder within a minute:\n%s", r.output())
}

// marker is the part of the run's names that holds its process ID.
func (r *run) marker() string {
	return "-" + strconv.Itoa(r.cmd.Process.Pid) + "-"
}

func (r *run) folder() string {
	matches, _ := filepath.Glob(filepath.Join(r.temp, prefix+"*"+r.marker()+"*"))
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// project is the run's Compose project, named like its folder.
func (r *run) project(t *testing.T) string {
	t.Helper()
	folder := r.folder()
	if folder == "" {
		t.Fatalf("the run has no folder:\n%s", r.output())
	}
	return filepath.Base(folder)
}

func (r *run) signal(t *testing.T, signal syscall.Signal) {
	t.Helper()
	if err := r.cmd.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
}

func (r *run) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-r.done:
		return r.status
	case <-time.After(runBound):
		t.Fatalf("the run did not end within %v:\n%s", runBound, r.output())
		return 0
	}
}

// removeAfter removes the run's project and folder, left behind on purpose, when the test ends; a failed removal
// fails the test and keeps the folder, which names the project.
func (r *run) removeAfter(t *testing.T) {
	folder := r.folder()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		down := exec.CommandContext(ctx, "docker", "compose", "-p", filepath.Base(folder), "down", "--volumes", "--remove-orphans", "--rmi", "local")
		if output, err := down.CombinedOutput(); err != nil {
			t.Errorf("remove the killed run's project %s: %v\n%s", filepath.Base(folder), err, output)
			return
		}
		if err := os.RemoveAll(folder); err != nil {
			t.Errorf("remove the killed run's folder: %v", err)
		}
	})
}

// assertGone checks that the run left neither its project, nor a volume of it, nor its folder.
func assertGone(t *testing.T, r *run) {
	t.Helper()
	if folder := r.folder(); folder != "" {
		t.Errorf("the run left its folder %s", folder)
	}
	// Compose lists a project by its containers: a volume, network or built image left without one is checked by the
	// project label Compose gives it, an image by its name, which starts with the project's.
	left := docker(t, "compose", "ls", "--all", "--quiet")
	for _, kind := range []string{"volume", "network"} {
		left = append(left, docker(t, kind, "ls", "--format", "{{.Name}}", "--filter", "label=com.docker.compose.project")...)
	}
	left = append(left, docker(t, "image", "ls", "--format", "{{.Repository}}", "--filter", "label=com.docker.compose.project")...)
	for _, name := range left {
		if strings.HasPrefix(name, prefix) && strings.Contains(name, r.marker()) {
			t.Errorf("the run left %s", name)
		}
	}
}

// failingDown is a folder holding a docker that fails only Compose's down and passes everything else to the real one.
func failingDown(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	// test-policy: the real docker cannot be made to fail one removal on demand; this stand-in fails only "down".
	script := "#!/bin/sh\ncase \" $* \" in *\" down \"*) echo 'down refused' >&2; exit 1;; esac\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(folder, "docker"), []byte(script), 0o500); err != nil {
		t.Fatal(err)
	}
	return folder
}

// postgresImage is the module's PostgreSQL image, read from its Compose file, so a built image costs no download.
func postgresImage(t *testing.T) string {
	t.Helper()
	file, err := os.ReadFile("compose.postgres.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(file)) {
		if image, ok := strings.CutPrefix(strings.TrimSpace(line), "image: "); ok {
			return image
		}
	}
	t.Fatal("compose.postgres.yaml names no image")
	return ""
}

func listed(t *testing.T, project string) bool {
	t.Helper()
	return slices.Contains(docker(t, "compose", "ls", "--all", "--quiet"), project)
}

// docker runs a listing and answers its lines.
func docker(t *testing.T, args ...string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return strings.Fields(string(output))
}
