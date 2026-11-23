package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	caddyDir  = "/etc/caddy/dgett"
	caddyfile = "/etc/caddy/Caddyfile"
)

type route struct {
	commit string
	branch string
	file   string
}

var routes = map[string]*route{}

var (
	nonLabel = regexp.MustCompile(`[^a-z0-9-]+`)
	dashes   = regexp.MustCompile(`-+`)
	hex7     = regexp.MustCompile(`^[0-9a-f]{7}$`)
)

func label(s string) string {
	s = strings.ToLower(s)
	s = nonLabel.ReplaceAllString(s, "-")
	s = dashes.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

type plan struct {
	app    string
	branch string
	hosts  []string
	files  map[string]string
}

func planRoutes(req Request, cfg Config) (plan, error) {
	name := cfg.Name
	if name == "" {
		name = req.Repo
	}
	p := plan{app: label(name), files: map[string]string{}}
	if p.app == "" {
		return p, fmt.Errorf("cannot make an app name from %q, set name in .dgett", name)
	}

	if req.Dirty || req.Branch == "" {
		h := req.Commit[:7] + "." + p.app + ".localhost"
		p.hosts = append(p.hosts, h)
		p.files[h] = p.app + "--" + req.Commit[:7] + ".caddy"
		return p, nil
	}

	bl := label(req.Branch)
	if bl == "" {
		return p, fmt.Errorf("cannot make a URL label from branch %q", req.Branch)
	}
	if hex7.MatchString(bl) {
		return p, fmt.Errorf("branch %q gives label %q, which looks like a commit digest", req.Branch, bl)
	}
	p.branch = req.Branch
	h := bl + "." + p.app + ".localhost"
	p.hosts = append(p.hosts, h)
	p.files[h] = p.app + "--" + bl + ".caddy"

	if cfg.Master != "" && req.Branch == cfg.Master {
		h := p.app + ".localhost"
		p.hosts = append(p.hosts, h)
		p.files[h] = p.app + ".caddy"
	}

	mu.Lock()
	defer mu.Unlock()
	for _, h := range p.hosts[:1] {
		if r := routes[h]; r != nil && r.branch != p.branch {
			return p, fmt.Errorf("branches %q and %q both map to %s", r.branch, p.branch, h)
		}
	}
	return p, nil
}

func applyRoutes(d *Deploy, p plan) ([]string, error) {
	changes := map[string][]byte{}
	for _, h := range p.hosts {
		changes[filepath.Join(caddyDir, p.files[h])] = []byte(fmt.Sprintf("%s {\n\treverse_proxy unix/%s\n}\n", h, d.Socket))
	}
	if err := applyCaddy(changes); err != nil {
		return nil, err
	}

	var urls []string
	replaced := map[string]bool{}
	mu.Lock()
	for _, h := range p.hosts {
		if r := routes[h]; r != nil && r.commit != d.Commit {
			replaced[r.commit] = true
		}
		routes[h] = &route{commit: d.Commit, branch: p.branch, file: p.files[h]}
		urls = append(urls, "https://"+h)
	}
	for c := range replaced {
		for _, r := range routes {
			if r.commit == c {
				delete(replaced, c)
				break
			}
		}
	}
	mu.Unlock()

	for c := range replaced {
		stop(c)
	}
	return urls, nil
}

func unroute(commit string) error {
	changes := map[string][]byte{}
	var hosts []string
	mu.Lock()
	for h, r := range routes {
		if r.commit == commit {
			hosts = append(hosts, h)
			changes[filepath.Join(caddyDir, r.file)] = nil
		}
	}
	mu.Unlock()
	if len(changes) == 0 {
		return nil
	}
	if err := applyCaddy(changes); err != nil {
		return err
	}
	mu.Lock()
	for _, h := range hosts {
		delete(routes, h)
	}
	mu.Unlock()
	return nil
}

func hostsOf(commit string) []string {
	var hosts []string
	for h, r := range routes {
		if r.commit == commit {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func applyCaddy(changes map[string][]byte) error {
	if err := os.MkdirAll(caddyDir, 0o755); err != nil {
		return err
	}
	old := map[string][]byte{}
	restore := func() {
		for p, c := range old {
			if c == nil {
				os.Remove(p)
			} else {
				os.WriteFile(p, c, 0o644)
			}
		}
	}
	for p, c := range changes {
		prev, err := os.ReadFile(p)
		if err != nil {
			prev = nil
		}
		old[p] = prev
		if c == nil {
			os.Remove(p)
		} else if err := os.WriteFile(p, c, 0o644); err != nil {
			restore()
			return err
		}
	}
	if out, err := exec.Command("caddy", "validate", "--config", caddyfile).CombinedOutput(); err != nil {
		restore()
		return fmt.Errorf("caddy validate failed:\n%s", out)
	}
	if out, err := exec.Command("caddy", "reload", "--config", caddyfile).CombinedOutput(); err != nil {
		restore()
		return fmt.Errorf("caddy reload failed:\n%s", out)
	}
	return nil
}

func waitSocket(d *Deploy) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(d.Socket); err == nil && fi.Mode()&fs.ModeSocket != 0 {
			return os.Chmod(d.Socket, 0o660)
		}
		select {
		case <-d.done:
			return errors.New("app exited before creating its socket")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("app did not create " + d.Socket + " within 5s")
}

func logTail(commit string, from int64, n int) string {
	data, err := os.ReadFile(filepath.Join(logsDir, commit+".log"))
	if err != nil {
		return ""
	}
	if from > 0 && from <= int64(len(data)) {
		data = data[from:]
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
