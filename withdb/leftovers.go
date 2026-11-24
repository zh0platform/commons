package withdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// owner is the start of this product's project names on this machine: the product's prefix and a tag of the host
// name and, on Linux, the process namespace, so a Docker daemon shared with another machine or container never makes
// process IDs this one cannot see look dead here.
func owner(p Product) string {
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
func reportLeftovers(stop context.Context, p Product, environ []string) {
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
