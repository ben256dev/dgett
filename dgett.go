package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/ben256dev/dgett/dgettenv"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time"
)

//go:embed templates/*.dgett
var templates embed.FS

const sockPath = "/run/dgett/dgett.sock"
const repoPath = "/var/lib/dgett/repo.git"

var (
	mu      sync.Mutex
	deploys = map[string]*Deploy{}
)

const (
	buildsDir = "/var/lib/dgett/builds"
	runDir    = "/run/dgett"
	logsDir   = "/var/log/dgett"
	dataDir   = "/var/lib/dgett/data"
	envDir    = "/etc/dgett/env"
	appRunDir = "/run/dgett/apps"
	keysDir   = "/var/lib/dgett/keys"
)

var gitMu sync.Mutex

var opMu sync.Mutex

type Deploy struct {
	Commit  string    `json:"commit"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Socket  string    `json:"socket"`
	Running bool      `json:"running"`
	Hosts   []string  `json:"hosts,omitempty"`
	EnvFile string    `json:"env_file"`
	Watch   bool      `json:"watch,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	Path    string    `json:"path,omitempty"`
	done    chan struct{}
	w       *watcher
}

type Request struct {
	Cmd    string `json:"cmd"`
	Commit string `json:"commit"`
	Config string `json:"config"`
	Branch string `json:"branch"`
	Dirty  bool   `json:"dirty"`
	Repo   string `json:"repo"`
	App    string `json:"app"`
	Path   string `json:"path,omitempty"`
}

type Response struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	Hint    string   `json:"hint,omitempty"`
	Deploys []Deploy `json:"deploys,omitempty"`
	URLs     []string `json:"urls,omitempty"`
	Template string   `json:"template,omitempty"`
}

type Config struct {
	Name         string   `toml:"name"`
	Master       string   `toml:"master"`
	Dependencies []string `toml:"dependencies"`
	Build        string   `toml:"build"`
	Run          string   `toml:"run"`
	Rebuild      string   `toml:"rebuild"`
	Watch        []string `toml:"watch"`
}

const configHint = "\nexample .dgett:\n" + configTemplate

const configTemplate = `dependencies = ["go"]
build = "go build -o app ."
run = "./app"
`

var appCred *syscall.Credential
var appUser string

func loadAppUser() {
	u, err := user.Lookup("dgett")
	if err != nil {
		log.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	appCred = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	appUser = u.Username
}

func export(commit, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chown(dir, int(appCred.Uid), int(appCred.Gid)); err != nil {
		return err
	}

	archive := exec.Command("git", "--git-dir="+repoPath, "archive", commit)
	extract := exec.Command("tar", "-x", "-C", dir)
	extract.SysProcAttr = &syscall.SysProcAttr{Credential: appCred}

	var archiveErr, extractErr bytes.Buffer
	archive.Stderr = &archiveErr
	extract.Stderr = &extractErr

	pipe, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	extract.Stdin = pipe

	if err := extract.Start(); err != nil {
		return err
	}
	if err := archive.Run(); err != nil {
		extract.Wait()
		return fmt.Errorf("git archive: %v: %s", err, archiveErr.String())
	}
	if err := extract.Wait(); err != nil {
		return fmt.Errorf("tar: %v: %s", err, extractErr.String())
	}
	return nil
}

func deploy(req Request, cfg Config) Response {
	p, err := planRoutes(req, cfg)
	if err != nil {
		return Response{Error: err.Error()}
	}

	mu.Lock()
	d, exists := deploys[req.Commit]
	mu.Unlock()
	if !exists {
		var resp Response
		if d, resp = start(req.Commit, p.app, "https://"+p.hosts[0], cfg); d == nil {
			return resp
		}
	}

	urls, err := applyRoutes(d, p)
	if err != nil {
		if !exists {
			stop(req.Commit)
		}
		return Response{Error: err.Error()}
	}
	return Response{OK: true, URLs: urls}
}

func appDirs(app string) (data, env string, err error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return "", "", err
	}
	if err := os.Chown(dataDir, 0, int(appCred.Gid)); err != nil {
		return "", "", err
	}
	data = filepath.Join(dataDir, app)
	if err := os.MkdirAll(data, 0o750); err != nil {
		return "", "", err
	}
	if err := os.Chown(data, int(appCred.Uid), int(appCred.Gid)); err != nil {
		return "", "", err
	}
	cache := filepath.Join(cacheDir, app)
	if err := os.MkdirAll(cache, 0o750); err != nil {
		return "", "", err
	}
	if err := os.Chown(cache, int(appCred.Uid), int(appCred.Gid)); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		return "", "", err
	}
	env = filepath.Join(envDir, app+".env")
	f, err := os.OpenFile(env, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err == nil {
		f.Close()
		if err := os.Chown(env, 0, int(appCred.Gid)); err != nil {
			return "", "", err
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return "", "", err
	}
	return data, env, nil
}

func start(commit, app, url string, cfg Config) (*Deploy, Response) {
	data, env, err := appDirs(app)
	if err != nil {
		return nil, Response{Error: "app folders: " + err.Error()}
	}

	dir := filepath.Join(buildsDir, commit)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := export(commit, dir); err != nil {
			os.RemoveAll(dir)
			return nil, Response{Error: "export failed: " + err.Error()}
		}
	}

	cache := filepath.Join(cacheDir, app)
	var buildTime time.Duration
	if cfg.Build != "" {
		build := exec.Command("sh", "-c", cfg.Build)
		build.Dir = dir
		build.SysProcAttr = &syscall.SysProcAttr{Credential: appCred}
		build.Env = append(os.Environ(), "HOME=/var/lib/dgett/home", "DGETT_BUILD=1", "DGETT_CACHE="+cache)
		t := time.Now()
		out, err := build.CombinedOutput()
		buildTime = time.Since(t)
		if err != nil {
			return nil, Response{Error: "build failed:\n" + string(out)}
		}
	}

	os.MkdirAll(logsDir, 0o755)
	if err := os.MkdirAll(appRunDir, 0o750); err != nil {
		return nil, Response{Error: "socket folder: " + err.Error()}
	}
	if err := os.Chown(appRunDir, int(appCred.Uid), int(appCred.Gid)); err != nil {
		return nil, Response{Error: "socket folder: " + err.Error()}
	}
	sock := filepath.Join(appRunDir, commit+".sock")
	report := filepath.Join(appRunDir, commit+".report")
	os.Remove(sock)
	os.Remove(report)

	logf, err := os.OpenFile(filepath.Join(logsDir, commit+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, Response{Error: "log file: " + err.Error()}
	}
	logStart, _ := logf.Seek(0, io.SeekEnd)
	if cfg.Build != "" {
		fmt.Fprintf(logf, "dgett: build %.1fs\n", buildTime.Seconds())
	}

	run := exec.Command("sh", "-c", cfg.Run)
	run.Dir = dir
	run.Env = append(os.Environ(), "DGETT_SOCKET="+sock, "DGETT_DATA="+data, "DGETT_CACHE="+cache, "DGETT_ENV="+env, "DGETT_REPORT="+report, "DGETT_URL="+url)
	run.Stdout, run.Stderr = logf, logf
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: appCred}
	if err := run.Start(); err != nil {
		logf.Close()
		return nil, Response{Error: "start failed: " + err.Error()}
	}

	d := &Deploy{
		Commit:  commit,
		PID:     run.Process.Pid,
		Started: time.Now(),
		Socket:  sock,
		EnvFile: env,
		done:    make(chan struct{}),
	}
	go func() {
		run.Wait()
		logf.Close()
		close(d.done)
	}()

	mu.Lock()
	deploys[commit] = d
	mu.Unlock()

	if err := waitSocket(d, 5*time.Second); err != nil {
		tail := logTail(commit, logStart, 20)
		stop(commit)
		if r, ok := readReport(report, app); ok && len(r.MissingEnv) > 0 {
			return nil, missingEnvError(r, app)
		}
		return nil, Response{Error: "start failed: " + err.Error() + "\n" + tail}
	}
	readReport(report, app)
	return d, Response{OK: true}
}

func readReport(path, app string) (dgettenv.Report, bool) {
	var r dgettenv.Report
	data, err := os.ReadFile(path)
	os.Remove(path)
	if err != nil || json.Unmarshal(data, &r) != nil {
		return r, false
	}
	if len(r.Keys) > 0 {
		if keys, err := json.Marshal(r.Keys); err == nil && os.MkdirAll(keysDir, 0o755) == nil {
			os.WriteFile(filepath.Join(keysDir, app+".json"), keys, 0o644)
		}
	}
	return r, true
}

func envTemplate(app string) Response {
	if label(app) != app || app == "" {
		return Response{Error: fmt.Sprintf("invalid app name %q", app)}
	}
	data, err := os.ReadFile(filepath.Join(keysDir, app+".json"))
	if err != nil {
		return Response{Error: "no env template for " + app + " yet, deploy it with dgett first"}
	}
	var keys []dgettenv.Key
	if err := json.Unmarshal(data, &keys); err != nil {
		return Response{Error: "env template for " + app + ": " + err.Error()}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", filepath.Join(envDir, app+".env"))
	for _, k := range keys {
		if k.Required {
			b.WriteString(k.Name + "=\n")
		}
	}
	b.WriteString("\n")
	for _, k := range keys {
		if !k.Required {
			fmt.Fprintf(&b, "%s=%s\n", k.Name, k.Default)
		}
	}
	return Response{OK: true, Template: b.String()}
}

func missingEnvError(r dgettenv.Report, app string) Response {
	const shown = 3
	var red []string
	for i, k := range r.MissingEnv {
		if i == shown {
			red = append(red, "...")
			break
		}
		red = append(red, "\033[1;31m"+k+"\033[0m")
	}
	noun := "env keys"
	if len(r.MissingEnv) == 1 {
		noun = "env key"
	}
	hint := ""
	if len(r.Keys) > 0 {
		hint = fmt.Sprintf("\033[38;5;136m%-40s# for .env template\033[0m\n", "dgett "+app+".env")
	}
	if fi, err := os.Stat(r.EnvFile); err == nil && fi.Size() == 0 && strings.HasPrefix(r.EnvFile, envDir+"/") {
		return Response{
			Error: "\033[94m" + r.EnvFile + "\033[0m was automatically created. It is empty and needs populated:",
			Hint:  hint,
		}
	}
	return Response{
		Error: fmt.Sprintf("missing \033[1;31m%d\033[0m ", len(r.MissingEnv)) + noun + " " + strings.Join(red, ", ") + " in \033[94m" + r.EnvFile + "\033[0m:",
		Hint:  hint,
	}
}

func remove(commit string) Response {
	mu.Lock()
	_, ok := deploys[commit]
	mu.Unlock()
	if !ok {
		return Response{Error: "not deployed: " + commit}
	}
	if err := unroute(commit); err != nil {
		return Response{Error: err.Error()}
	}
	stop(commit)
	return Response{OK: true}
}

func stop(commit string) {
	mu.Lock()
	d, ok := deploys[commit]
	delete(deploys, commit)
	var pid int
	var done chan struct{}
	var draining []*gen
	if ok {
		pid, done = d.PID, d.done
		if d.w != nil {
			close(d.w.quit)
			draining = append(draining, d.w.draining...)
		}
	}
	mu.Unlock()
	if !ok {
		return
	}

	for _, g := range draining {
		syscall.Kill(-g.pid, syscall.SIGKILL)
	}
	killGroup(pid, done)
	os.Remove(d.Socket)
	if d.w != nil {
		return
	}

	gitMu.Lock()
	os.RemoveAll(filepath.Join(buildsDir, commit))
	exec.Command("git", "--git-dir="+repoPath, "update-ref", "-d", "refs/dgett/"+commit).Run()
	gitMu.Unlock()
}

func killGroup(pid int, done chan struct{}) {
	syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		syscall.Kill(-pid, syscall.SIGKILL)
		<-done
	}
}

func list() Response {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Deploy, 0, len(deploys))
	for _, d := range deploys {
		c := *d
		select {
		case <-d.done:
			c.Running = false
		default:
			c.Running = true
		}
		c.Hosts = hostsOf(d.Commit)
		sort.Strings(c.Hosts)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return Response{OK: true, Deploys: out}
}

func handle(conn net.Conn) {
	defer conn.Close()
	enc := json.NewEncoder(conn)

	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		enc.Encode(Response{Error: "bad request: " + err.Error()})
		return
	}

	switch req.Cmd {
	case "deploy":
		if resp := checkCommit(req.Commit); resp != nil {
			enc.Encode(resp)
			return
		}
		cfg, resp := parseConfig(req.Config)
		if resp != nil {
			enc.Encode(resp)
			return
		}
		if missing := missingDeps(cfg.Dependencies); len(missing) > 0 {
			host, _ := os.Hostname()
			red := make([]string, len(missing))
			for i, m := range missing {
				red[i] = "\033[1;31m" + m + "\033[0m"
			}
			noun := "dependencies"
			if len(missing) == 1 {
				noun = "dependency"
			}
			enc.Encode(Response{Error: fmt.Sprintf("missing \033[1;31m%d\033[0m ", len(missing)) + noun + " " + strings.Join(red, ", ") + " for user \033[94m" + appUser + "@" + host + "\033[0m:", Hint: installHint(missing)})
			return
		}
		opMu.Lock()
		out := deploy(req, cfg)
		opMu.Unlock()
		enc.Encode(out)

	case "watch":
		cfg, resp := parseConfig(req.Config)
		if resp != nil {
			enc.Encode(resp)
			return
		}
		if missing := missingDeps(cfg.Dependencies); len(missing) > 0 {
			enc.Encode(Response{Error: fmt.Sprintf("missing \033[1;31m%d\033[0m dependencies: ", len(missing)) + strings.Join(missing, ", "), Hint: installHint(missing)})
			return
		}
		uid, err := peerUID(conn)
		if err != nil {
			enc.Encode(Response{Error: "peer credentials: " + err.Error()})
			return
		}
		opMu.Lock()
		out := watchDeploy(req, cfg, uid)
		opMu.Unlock()
		enc.Encode(out)

	case "rm":
		mu.Lock()
		d := deploys[req.Commit]
		mu.Unlock()
		if d == nil || d.w == nil {
			if resp := checkCommit(req.Commit); resp != nil {
				enc.Encode(resp)
				return
			}
		}
		opMu.Lock()
		resp := remove(req.Commit)
		opMu.Unlock()
		enc.Encode(resp)

	case "env":
		enc.Encode(envTemplate(req.App))

	case "ls":
		enc.Encode(list())

	default:
		enc.Encode(Response{Error: fmt.Sprintf("unknown command %q", req.Cmd)})
	}
}

func git(env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func snapshot() (string, bool, error) {
	gitDir, err := git(nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", false, err
	}
	idxPath := filepath.Join(gitDir, "dgett-index")
	defer os.Remove(idxPath)
	idx := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, err := git(idx, "read-tree", "HEAD"); err != nil {
		return "", false, err
	}
	if _, err := git(idx, "add", "-A"); err != nil {
		return "", false, err
	}
	tree, err := git(idx, "write-tree")
	if err != nil {
		return "", false, err
	}
	headTree, err := git(nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", false, err
	}
	if tree == headTree {
		head, err := git(nil, "rev-parse", "HEAD")
		return head, true, err
	}
	date, err := git(nil, "show", "-s", "--format=%cI", "HEAD")
	if err != nil {
		return "", false, err
	}
	env := []string{
		"GIT_AUTHOR_NAME=dgett", "GIT_AUTHOR_EMAIL=dgett@localhost", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=dgett", "GIT_COMMITTER_EMAIL=dgett@localhost", "GIT_COMMITTER_DATE=" + date,
	}
	dgst, err := git(env, "commit-tree", tree, "-p", "HEAD", "-m", "dgett snapshot")
	return dgst, false, err
}

func push(commit string) error {
	_, err := git(nil, "push", "--quiet", repoPath, commit+":refs/dgett/"+commit)
	return err
}

func checkout(commit string) error {
	_, err := git(nil, "checkout", "--quiet", commit, "--", repoPath)
	return err
}

func create_socket() net.Listener {
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatal("failed to Listen", sockPath)
	}
	g, err := user.LookupGroup("dgett-users")
	if err != nil {
		log.Fatal("lookup group \"dgett-users\" failed", err)
	}
	gid, _ := strconv.Atoi(g.Gid)
	if err := os.Chown(sockPath, -1, gid); err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(sockPath, 0o660); err != nil {
		log.Fatal(err)
	}
	return ln
}

func watch_request(conn net.Conn) bool {
	top, err := git(nil, "rev-parse", "--show-toplevel")
	if err != nil {
		log.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(top, ".dgett"))
	if err != nil {
		return false
	}
	cfg, resp := parseConfig(string(data))
	if resp != nil {
		return false
	}
	branch, _ := git(nil, "symbolic-ref", "--short", "-q", "HEAD")
	if !watched(cfg.Watch, branch) {
		return false
	}
	json.NewEncoder(conn).Encode(Request{
		Cmd:    "watch",
		Config: string(data),
		Branch: branch,
		Repo:   filepath.Base(top),
		Path:   top,
	})
	return true
}

func push_snapshot(conn net.Conn) string {
	dgst, clean, err := snapshot()
	if err != nil {
		log.Fatal(err)
	}
	branch, _ := git(nil, "symbolic-ref", "--short", "-q", "HEAD")

	push(dgst)

	top, err := git(nil, "rev-parse", "--show-toplevel")

	data, err := os.ReadFile(filepath.Join(top, ".dgett"))
	if err != nil {
		log.Fatal("no .dgett file")
	}

	json.NewEncoder(conn).Encode(Request{
		Cmd:    "deploy",
		Commit: dgst,
		Config: string(data),
		Branch: branch,
		Dirty:  !clean,
		Repo:   filepath.Base(top),
	})

	return dgst
}

var hashRegexp = regexp.MustCompile(`^[0-9a-f]{40}$`)

func checkCommit(commit string) *Response {
	if !hashRegexp.MatchString(commit) {
		return &Response{Error: fmt.Sprintf("invalid commit %q", commit)}
	}
	check := exec.Command("git", "--git-dir="+repoPath, "cat-file", "-e", commit+"^{commit}")
	if check.Run() != nil {
		return &Response{Error: "commit " + commit [:7] + " not in repo, did the push fail?"}
	}
	return nil
}

func missingDeps(deps []string) []string {
	var missing []string
	for _, d := range deps {
		if _, err := exec.LookPath(d); err != nil {
			missing = append(missing, d)
		}
	}
	return missing
}

func parseConfig(raw string) (Config, *Response) {
	var cfg Config
	if strings.TrimSpace(raw) == "" {
		return cfg, &Response{Error: "missing .dgett", Hint: configHint}
	}
	md, err := toml.Decode(raw, &cfg)
	var perr toml.ParseError
	if errors.As(err, &perr) {
		return cfg, &Response{Error: ".dgett: " + perr.ErrorWithPosition()}
	}
	if err != nil {
		return cfg, &Response{Error: ".dgett: " + perr.Error()}
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		return cfg, &Response{Error: fmt.Sprintf(".dgett: unknown key %q", extra[0].String()), Hint: configHint}
	}
	if cfg.Run == "" {
		return cfg, &Response{Error: ".dgett: run is required", Hint: configHint}
	}
	for _, p := range cfg.Watch {
		if _, err := pathpkg.Match(p, ""); err != nil {
			return cfg, &Response{Error: fmt.Sprintf(".dgett: bad watch pattern %q", p)}
		}
	}
	return cfg, nil
}

func parsecommand() string {
	if len(os.Args) < 2 {
		return ""
	}
	return os.Args[1]
}

func request(req Request) Response {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Fatal("daemon not running: ", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		log.Fatal("send: ", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		log.Fatal("no reply from daemon: ", err)
	}
	if !resp.OK {
		fmt.Fprintln(os.Stderr, resp.Error)
		if resp.Hint != "" {
			fmt.Fprintf(os.Stderr, "\n%s", resp.Hint)
		}
		os.Exit(1)
	}
	return resp
}

func main() {
	switch cmd := parsecommand(); cmd {
	case "--daemon":
		loadAppUser()

		ln := create_socket()
		serveProxy()
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Fatalf("failed to Accept", sockPath)
			}
			go handle(conn)
		}

	case "":
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			log.Fatal("daemon not running: ", err)
		}
		defer conn.Close()

		if !watch_request(conn) {
			push_snapshot(conn)
		}

		var resp Response
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			log.Fatal("no reply from daemon: ", err)
		}
		if !resp.OK {
			fmt.Fprintln(os.Stderr, resp.Error)
			if resp.Hint != "" {
				fmt.Fprint(os.Stderr, resp.Hint)
			}
			os.Exit(1)
		}
		for _, u := range resp.URLs {
			fmt.Println(u)
		}

	case "ls":
		resp := request(Request{Cmd: "ls"})
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "COMMIT\tSTATE\tPID\tSTARTED\tENV\tHOSTS")
		for _, d := range resp.Deploys {
			state := "stopped"
			if d.Running {
				state = "running"
			}
			commit := d.Commit
			if d.Watch {
				commit = "watch"
			} else {
				commit = commit[:7]
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", commit, state, d.PID, d.Started.Format("2006-01-02 15:04"), d.EnvFile, strings.Join(d.Hosts, " "))
		}
		w.Flush()

	case "rm":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: dgett rm <commit|branch>")
			os.Exit(2)
		}
		prefix := os.Args[2]
		var matches []string
		for _, d := range request(Request{Cmd: "ls"}).Deploys {
			if d.Watch {
				if prefix == d.Branch || prefix == label(d.Branch) {
					request(Request{Cmd: "rm", Commit: d.Commit})
					fmt.Println("removed", prefix)
					return
				}
				continue
			}
			if strings.HasPrefix(d.Commit, prefix) {
				matches = append(matches, d.Commit)
			}
		}
		switch len(matches) {
		case 0:
			fmt.Fprintf(os.Stderr, "no deploy matches %s\n", prefix)
			os.Exit(1)
		case 1:
			request(Request{Cmd: "rm", Commit: matches[0]})
			fmt.Println("removed", matches[0][:7])
		default:
			fmt.Fprintf(os.Stderr, "%s is ambiguous: %s\n", prefix, strings.Join(matches, ", "))
			os.Exit(1)
		}

	case "basic.dgett", "full.dgett":
		data, err := templates.ReadFile("templates/" + cmd)
		if err != nil {
			log.Fatal(err)
		}
		os.Stdout.Write(data)

	case "--help", "-h":
		usageMsg := `usage: dgett [COMMAND]...
  dgett               deploy repo over localhost
  dgett ls            list managed processes
  dgett rm            delete a process by commit or watched branch
  dgett <app>.env     print an env file template for an app
  dgett --daemon      run daemon
  dgett -h | --help   print this message

A .dgett file must be present to deploy. Templates available with "dgett basic.dgett" and "dgett full.dgett"`

		fmt.Println(usageMsg)

	default:
		if app, ok := strings.CutSuffix(cmd, ".env"); ok {
			fmt.Print(request(Request{Cmd: "env", App: app}).Template)
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
	}
}
