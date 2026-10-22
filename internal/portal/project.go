package portal

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// Detection describes how to run a project folder.
type Detection struct {
	Type    string `json:"type"`    // static, node, django, python, go, docker, unknown
	Command string `json:"command"` // empty for static sites; $PORT is replaced with the assigned port
	Note    string `json:"note"`
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func readFile(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return string(b)
}

var exposeRE = regexp.MustCompile(`(?mi)^\s*EXPOSE\s+(\d+)`)

// Detect looks at a folder and suggests how to run it.
func Detect(dir string) Detection {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return Detection{Type: "unknown", Note: "That folder doesn't exist on this computer."}
	}
	slug := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(filepath.Base(dir)), "-"), "-")
	if slug == "" {
		slug = "app"
	}
	switch {
	case exists(dir, "package.json"):
		var pkg struct {
			Scripts         map[string]string `json:"scripts"`
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		_ = json.Unmarshal([]byte(readFile(dir, "package.json")), &pkg)
		dep := func(n string) bool { _, a := pkg.Dependencies[n]; _, b := pkg.DevDependencies[n]; return a || b }
		install := ""
		if !exists(dir, "node_modules") {
			install = "npm install && "
		}
		switch {
		case pkg.Scripts["dev"] != "" && dep("vite"):
			return Detection{"node", install + "npm run dev -- --port $PORT --strictPort", "Vite project, run in development mode."}
		case pkg.Scripts["dev"] != "" && dep("next"):
			return Detection{"node", install + "npm run dev -- -p $PORT", "Next.js project, run in development mode."}
		case pkg.Scripts["start"] != "":
			return Detection{"node", install + "npm start", "Node.js project, started with its \"start\" script (it should read the PORT variable)."}
		case pkg.Scripts["dev"] != "":
			return Detection{"node", install + "npm run dev", "Node.js project, started with its \"dev\" script (it should read the PORT variable)."}
		}
		for _, f := range []string{"server.js", "index.js", "app.js", "main.js"} {
			if exists(dir, f) {
				return Detection{"node", install + "node " + f, "Node.js project (" + f + ")."}
			}
		}
	case exists(dir, "manage.py"):
		return Detection{"django", python() + " manage.py runserver 127.0.0.1:$PORT", "Django project. Install its requirements first if you haven't."}
	case exists(dir, "Dockerfile"):
		port := "80"
		if m := exposeRE.FindStringSubmatch(readFile(dir, "Dockerfile")); m != nil {
			port = m[1]
		}
		return Detection{"docker", "docker build -t universal-" + slug + " . && docker run --rm -p 127.0.0.1:$PORT:" + port + " universal-" + slug,
			"Docker project, built and run with Docker (Docker must be installed and running)."}
	case exists(dir, "go.mod"):
		return Detection{"go", "go run .", "Go project (it should read the PORT variable)."}
	}
	for _, f := range []string{"main.py", "app.py", "server.py"} {
		if !exists(dir, f) {
			continue
		}
		src := readFile(dir, f)
		mod := strings.TrimSuffix(f, ".py")
		switch {
		case strings.Contains(src, "FastAPI("):
			return Detection{"python", python() + " -m uvicorn " + mod + ":app --host 127.0.0.1 --port $PORT", "FastAPI app. Install its requirements first if you haven't."}
		case strings.Contains(src, "Flask("):
			return Detection{"python", python() + " -m flask --app " + mod + " run --port $PORT", "Flask app. Install its requirements first if you haven't."}
		case strings.Contains(src, "streamlit"):
			return Detection{"python", python() + " -m streamlit run " + f + " --server.port $PORT --server.headless true", "Streamlit app."}
		}
		return Detection{"python", python() + " " + f, "Python app (it should read the PORT variable)."}
	}
	if exists(dir, "index.html") {
		return Detection{Type: "static", Note: "Static website: the files are served as they are, nothing to run."}
	}
	return Detection{Type: "unknown", Note: "Couldn't tell how to start this project. Enter the command you normally use to start it."}
}

func python() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	if _, err := exec.LookPath("python3"); err == nil {
		return "python3"
	}
	return "python"
}

// logBuffer keeps the last lines a project printed.
type logBuffer struct {
	mu      sync.Mutex
	lines   []string
	partial string
}

const maxLogLines = 500

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	parts := strings.Split(l.partial+strings.ReplaceAll(string(p), "\r\n", "\n"), "\n")
	l.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		l.add(line)
	}
	return len(p), nil
}

func (l *logBuffer) add(line string) {
	l.lines = append(l.lines, line)
	if len(l.lines) > maxLogLines {
		l.lines = l.lines[len(l.lines)-maxLogLines:]
	}
}

// Note appends a line from the portal itself.
func (l *logBuffer) Note(s string) { _, _ = l.Write([]byte("[portal] " + s + "\n")) }

func (l *logBuffer) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]string(nil), l.lines...)
	if l.partial != "" {
		out = append(out, l.partial)
	}
	return out
}
