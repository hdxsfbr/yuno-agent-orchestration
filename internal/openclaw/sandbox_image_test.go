package openclaw

import (
	"os"
	"strings"
	"testing"
)

// The P3 acceptance for the images (D-082): libgomp1, the OpenMP runtime the numpy,
// scikit-learn, LightGBM and XGBoost wheels load, is installed in the base sandbox image
// and in the Python toolchain image, so an agent's `import lightgbm` needs no workaround.
// The Dockerfiles are read from the repository, so dropping the package from either one
// fails the suite.
func TestSandboxImagesInstallLibgomp(t *testing.T) {
	for _, name := range []string{"sandbox.Dockerfile", "sandbox-python.Dockerfile"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("../../docker/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if !installsPackage(string(raw), "libgomp1") {
				t.Errorf("docker/%s does not apt-get install libgomp1 in its final image", name)
			}
		})
	}
}

// installsPackage reports whether the last build stage of a Dockerfile installs pkg with
// apt-get: comments, line continuations and commands joined by && or ; are read as the
// shell would, and a package in an earlier stage, which the final image does not carry,
// does not count.
func installsPackage(dockerfile, pkg string) bool {
	var instructions []string
	var current strings.Builder
	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if rest, more := strings.CutSuffix(line, "\\"); more {
			current.WriteString(rest + " ")
			continue
		}
		current.WriteString(line)
		instructions = append(instructions, current.String())
		current.Reset()
	}
	found := false
	for _, instr := range instructions {
		fields := strings.Fields(instr)
		switch {
		case len(fields) > 0 && strings.EqualFold(fields[0], "FROM"):
			found = false
		case len(fields) > 0 && strings.EqualFold(fields[0], "RUN"):
			shell := strings.NewReplacer("&&", ";", "||", ";").Replace(strings.Join(fields[1:], " "))
			for _, command := range strings.Split(shell, ";") {
				words := strings.Fields(command)
				if len(words) < 2 || words[0] != "apt-get" || words[1] != "install" {
					continue
				}
				for _, w := range words[2:] {
					if name, _, _ := strings.Cut(w, "="); name == pkg {
						found = true
					}
				}
			}
		}
	}
	return found
}

func TestInstallsPackage(t *testing.T) {
	for _, c := range []struct {
		name, dockerfile string
		want             bool
	}{
		{"on one line", "FROM python:3.12-slim\nRUN apt-get install -y git libgomp1\n", true},
		{"across continued lines", "FROM python:3.12-slim\nRUN apt-get update && apt-get install -y --no-install-recommends git \\\n    libgomp1 \\\n    && rm -rf /var/lib/apt/lists/*\n", true},
		{"pinned", "FROM x\nRUN apt-get install -y libgomp1=12.2.0-14\n", true},
		{"dropped", "FROM python:3.12-slim\nRUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \\\n    && rm -rf /var/lib/apt/lists/*\n", false},
		{"only in a comment", "FROM x\n# libgomp1 is needed\nRUN apt-get install -y git\n", false},
		{"only mentioned", "FROM x\nRUN apt-get install -y git && echo libgomp1\n", false},
		{"another package of the name", "FROM x\nRUN apt-get install -y libgomp10\n", false},
		{"in an earlier stage only", "FROM x AS build\nRUN apt-get install -y libgomp1\nFROM y\nRUN apt-get install -y git\n", false},
		{"in the last stage", "FROM x AS build\nRUN apt-get install -y git\nFROM y\nRUN apt-get install -y libgomp1\n", true},
	} {
		if got := installsPackage(c.dockerfile, "libgomp1"); got != c.want {
			t.Errorf("%s: installsPackage = %v, want %v", c.name, got, c.want)
		}
	}
}
