package main

import (
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

//go:embed deps.toml
var embeddedTools string

const (
	toolsOverride = "/etc/dgett/deps.toml"
	toolsURL      = "https://dgett.dev/deps.toml"
)

type toolsFile struct {
	Version string                       `toml:"version"`
	Tools   map[string]map[string]string `toml:"tools"`
}

var packageManagers = []struct{ name, install string }{
	{"apt", "sudo apt install"},
	{"dnf", "sudo dnf install"},
	{"pacman", "sudo pacman -S"},
}

func parseTools(raw string) map[string]map[string]string {
	var f toolsFile
	if _, err := toml.Decode(raw, &f); err != nil {
		return nil
	}
	return f.Tools
}

func fetchTools() string {
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get(toolsURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	return string(body)
}

// installHint says how to install each missing tool. Sources are checked in
// order (override file, dgett.dev, embedded copy); the first one that knows a
// tool wins.
func installHint(missing []string) string {
	var sources []map[string]map[string]string
	if raw, err := os.ReadFile(toolsOverride); err == nil {
		sources = append(sources, parseTools(string(raw)))
	}
	sources = append(sources, parseTools(fetchTools()), parseTools(embeddedTools))

	pm := ""
	install := ""
	for _, p := range packageManagers {
		if _, err := exec.LookPath(p.name); err == nil {
			pm, install = p.name, p.install
			break
		}
	}

	const yellow = "\033[38;5;136m"
	const reset = "\033[0m"
	type line struct{ cmd, url string }
	var lines []line
	width := 40
	for _, name := range missing {
		for _, s := range sources {
			t, ok := s[name]
			if !ok {
				continue
			}
			l := line{url: t["url"]}
			if pkg := t[pm]; pkg != "" {
				l.cmd = install + " " + pkg
			}
			if l.cmd != "" || l.url != "" {
				lines = append(lines, l)
				width = max(width, len(l.cmd)+4)
			}
			break
		}
	}

	var b strings.Builder
	for _, l := range lines {
		if l.url == "" {
			b.WriteString(yellow + l.cmd + reset + "\n")
			continue
		}
		fmt.Fprintf(&b, "%s%-*s# %s%s\n", yellow, width, l.cmd, l.url, reset)
	}
	return b.String()
}
