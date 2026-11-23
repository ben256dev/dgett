package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/BurntSushi/toml"
	_ "io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
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
)

var gitMu sync.Mutex

type Deploy struct {
	Commit  string    `json:"commit"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Socket  string    `json:"socket"`
	Running bool      `json:"running"`
	done    chan struct{}
}

type Request struct {
	Cmd    string `json:"cmd"`
	Commit string `json:"commit"`
	Config string `json:"config"`
}

type Response struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	Hint    string   `json:"hint,omitempty"`
	Deploys []Deploy `json:"deploys,omitempty"`
}

type Config struct {
	Dependencies []string `toml:"dependencies"`
	Build        string   `toml:"build"`
	Run          string   `toml:"run"`
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

func deploy(commit string, cfg Config) Response {
	mu.Lock()
	_, exists := deploys[commit]
	mu.Unlock()
	if exists {
		return Response{OK: true}
	}

	dir := filepath.Join(buildsDir, commit)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := export(commit, dir); err != nil {
			os.RemoveAll(dir)
			return Response{Error: "export failed: " + err.Error()}
		}
	}

	if cfg.Build != "" {
		build := exec.Command("sh", "-c", cfg.Build)
		build.Dir = dir
		build.SysProcAttr = &syscall.SysProcAttr{Credential: appCred}
		build.Env = append(os.Environ(), "HOME=/var/lib/dgett/home")
		if out, err := build.CombinedOutput(); err != nil {
			return Response{Error: "build failed:\n" + string(out)}
		}
	}

	os.MkdirAll(runDir, 0o755)
	os.MkdirAll(logsDir, 0o755)
	sock := filepath.Join(runDir, commit+".sock")
	os.Remove(sock)

	logf, err := os.OpenFile(filepath.Join(logsDir, commit+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Response{Error: "log file: " + err.Error()}
	}

	run := exec.Command("sh", "-c", cfg.Run)
	run.Dir = dir
	run.Env = append(os.Environ(), "DGETT_SOCKET="+sock)
	run.Stdout, run.Stderr = logf, logf
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: appCred}
	if err := run.Start(); err != nil {
		logf.Close()
		return Response{Error: "start failed: " + err.Error()}
	}

	d := &Deploy{
		Commit:  commit,
		PID:     run.Process.Pid,
		Started: time.Now(),
		Socket:  sock,
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
	return Response{OK: true}
}

func remove(commit string) Response {
	mu.Lock()
	d, ok := deploys[commit]
	delete(deploys, commit)
	mu.Unlock()
	if !ok {
		return Response{Error: "not deployed: " + commit[:7]}
	}

	syscall.Kill(-d.PID, syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(10 * time.Second):
		syscall.Kill(-d.PID, syscall.SIGKILL)
		<-d.done
	}
	os.Remove(d.Socket)

	gitMu.Lock()
	os.RemoveAll(filepath.Join(buildsDir, commit))
	exec.Command("git", "--git-dir="+repoPath, "update-ref", "-d", "refs/dgett/"+commit).Run()
	gitMu.Unlock()

	return Response{OK: true}
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
			enc.Encode(Response{Error: "missing " + noun + " " + strings.Join(red, ", ") + " for user \033[94m" + appUser + "@" + host + "\033[0m:", Hint: installHint(missing)})
			return
		}
		enc.Encode(deploy(req.Commit, cfg))

	case "rm":
		if resp := checkCommit(req.Commit); resp != nil {
			enc.Encode(resp)
			return
		}
		enc.Encode(remove(req.Commit))

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

func snapshot() (string, error) {
	gitDir, err := git(nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	idxPath := filepath.Join(gitDir, "dgett-index")
	defer os.Remove(idxPath)
	idx := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, err := git(idx, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := git(idx, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := git(idx, "write-tree")
	if err != nil {
		return "", err
	}
	headTree, err := git(nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	if tree == headTree {
		return git(nil, "rev-parse", "HEAD")
	}
	date, err := git(nil, "show", "-s", "--format=%cI", "HEAD")
	if err != nil {
		return "", err
	}
	env := []string{
		"GIT_AUTHOR_NAME=dgett", "GIT_AUTHOR_EMAIL=dgett@localhost", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=dgett", "GIT_COMMITTER_EMAIL=dgett@localhost", "GIT_COMMITTER_DATE=" + date,
	}
	return git(env, "commit-tree", tree, "-p", "HEAD", "-m", "dgett snapshot")
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

func push_snapshot(conn net.Conn) string {
	dgst, err := snapshot()
	if err != nil {
		log.Fatal(err)
	}

	push(dgst)

	top, err := git(nil, "rev-parse", "--show-toplevel")

	data, err := os.ReadFile(filepath.Join(top, ".dgett"))
	if err != nil {
		log.Fatal("no .dgett file")
	}

	json.NewEncoder(conn).Encode(Request{
		Cmd:    "deploy",
		Commit:   dgst,
		Config: string(data),
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

		dgst := push_snapshot(conn)

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
		fmt.Println("deployed", dgst[:7])

	case "ls":
		resp := request(Request{Cmd: "ls"})
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "COMMIT\tSTATE\tPID\tSTARTED")
		for _, d := range resp.Deploys {
			state := "stopped"
			if d.Running {
				state = "running"
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", d.Commit[:7], state, d.PID, d.Started.Format("2006-01-02 15:04"))
		}
		w.Flush()

	case "rm":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: dgett rm <commit>")
			os.Exit(2)
		}
		prefix := os.Args[2]
		var matches []string
		for _, d := range request(Request{Cmd: "ls"}).Deploys {
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

	case "--help", "-h":
		usageMsg := `usage: dgett [COMMAND]...
  dgett               deploy repo over localhost
  dgett ls            list managed processes
  dgett rm            delete a process
  dgett --daemon      run daemon
  dgett -h | --help   print this message

A .dgett file must be present to deploy. Templates available with "dgett basic.dgett" and "dgett full.dgett"`

		fmt.Println(usageMsg)

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
	}
}
