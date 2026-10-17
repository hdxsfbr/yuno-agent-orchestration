package project

import (
	"os"
	"path"
	"sort"
	"strings"
)

// Toolchain is what the manifest at the root of a checkout says the project is built
// with (DESIGN 5.16 rule 5): the name selects the sandbox image, the commands go into
// the task protocol block when the sandbox has network.
type Toolchain struct {
	Name    string // go | node | python | default
	Install string // how to install the project's dependencies; "" for default
	Test    string // how to run the project's tests; "" for default
}

// Toolchain names.
const (
	ToolchainGo      = "go"
	ToolchainNode    = "node"
	ToolchainPython  = "python"
	ToolchainDefault = "default"
)

// Toolchains are the known names, in the order of the detection table.
var Toolchains = []string{ToolchainGo, ToolchainNode, ToolchainPython, ToolchainDefault}

// Detect reads the root of a checkout and returns its toolchain, first match of the
// table in DESIGN 5.16 rule 5 winning. A directory that cannot be read is "default".
func Detect(dir string) Toolchain {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Toolchain{Name: ToolchainDefault}
	}
	have := map[string]bool{}
	var requirements []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		have[name] = true
		if strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt") {
			requirements = append(requirements, name)
		}
	}
	switch {
	case have["go.mod"]:
		return Toolchain{Name: ToolchainGo, Install: "go mod download", Test: "go test ./..."}
	case have["package.json"]:
		switch {
		case have["pnpm-lock.yaml"]:
			return Toolchain{Name: ToolchainNode, Install: "pnpm install --frozen-lockfile", Test: "pnpm test"}
		case have["yarn.lock"]:
			return Toolchain{Name: ToolchainNode, Install: "yarn install --frozen-lockfile", Test: "yarn test"}
		case have["package-lock.json"]:
			return Toolchain{Name: ToolchainNode, Install: "npm ci", Test: "npm test"}
		}
		return Toolchain{Name: ToolchainNode, Install: "npm install", Test: "npm test"}
	case have["uv.lock"]:
		return Toolchain{Name: ToolchainPython, Install: "uv sync", Test: "uv run pytest"}
	case len(requirements) > 0:
		sort.Slice(requirements, func(i, j int) bool {
			// requirements.txt first, then the rest by name.
			if (requirements[i] == "requirements.txt") != (requirements[j] == "requirements.txt") {
				return requirements[i] == "requirements.txt"
			}
			return requirements[i] < requirements[j]
		})
		install := "python -m venv .venv"
		for _, r := range requirements {
			install += " && .venv/bin/pip install -r " + r
		}
		return Toolchain{Name: ToolchainPython, Install: install, Test: ".venv/bin/pytest"}
	case have["pyproject.toml"]:
		return Toolchain{Name: ToolchainPython, Install: "python -m venv .venv && .venv/bin/pip install -e .", Test: ".venv/bin/pytest"}
	}
	return Toolchain{Name: ToolchainDefault}
}

// SandboxImage is the image for a toolchain, derived from the default sandbox image
// (rule 6): "yuno-sandbox:latest" and "go" give "yuno-sandbox-go:latest"; "default" and
// an unknown name keep the default image.
func SandboxImage(defaultImage, toolchain string) string {
	switch toolchain {
	case ToolchainGo, ToolchainNode, ToolchainPython:
	default:
		return defaultImage
	}
	name, tag := defaultImage, ""
	// The tag is after the last colon that follows the last slash (a registry port is
	// before the slash).
	if i := strings.LastIndex(defaultImage, ":"); i > strings.LastIndex(defaultImage, "/") {
		name, tag = defaultImage[:i], defaultImage[i:]
	}
	return name + "-" + toolchain + tag
}

// RepoDir is where the checkout lives inside a run directory (rule 3).
const RepoDir = "repo"

// RepoRel is the checkout's path relative to the agent's workspace.
func RepoRel(runID string) string { return path.Join("runs", runID, RepoDir) }

// Instructions are the task protocol lines of DESIGN 5.16 rule 9 for a visit of a
// project run: where the checkout is, what not to do, and what the sandbox network
// allows.
func Instructions(c Config, runID string, tc Toolchain) string {
	var sb strings.Builder
	sb.WriteString("Project repository: " + RepoRel(runID) + "/ in your workspace is a git checkout of " + c.RepoURL +
		" (branch " + c.DefaultBranch + ", toolchain " + tc.Name + "). Work inside it. Do not clone, commit or push: " +
		"the platform commits the working tree when a merge request is opened.\n")
	switch {
	case c.SandboxNetwork && tc.Install != "":
		sb.WriteString("Sandbox network: on. Install the dependencies from the lockfile first (" + tc.Install +
			"), then run the project's tests (" + tc.Test + ").\n")
	case c.SandboxNetwork:
		sb.WriteString("Sandbox network: on. No manifest was recognised; install what the project needs yourself and run its tests.\n")
	default:
		sb.WriteString("Sandbox network: off. Dependencies cannot be installed: use what the image provides and say which tests you could not run.\n")
	}
	return sb.String()
}
