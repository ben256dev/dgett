package main

import (

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
	"strconv"
	"strings"
	_ "time"
)

const sockPath = "/run/dgett/dgett.sock"
const repoPath = "/var/lib/dgett/repo.git"

type Request struct {
	Cmd    string `json:"cmd"`
	Commit string `json:"commit"`
	Config string `json:"config"`
}

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

type Config struct {
	Build string `toml:"build"`
	Run   string `toml:"run"`
}

const configTemplate = `build = "go build -o app ."
run = "./app"
`

func handle(conn net.Conn) {
	defer conn.Close()
	enc := json.NewEncoder(conn)

	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		enc.Encode(Response{Error: "bad request: " + err.Error()})
		return
	}

	cfg, resp := validate(req)
	if resp != nil {
		enc.Encode(resp)
		return
	}

	log.Printf("deploy %s: build=%q run=%q", req.Commit[:7], cfg.Build, cfg.Run)
	enc.Encode(Response{OK: true})
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
	return git(nil, "commit-tree", tree, "-p", "HEAD", "-m", "dgett snapshot")
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
	g, err := user.LookupGroup("dgett")
	if err != nil {
		log.Fatal("lookup group \"dgett\" failed", err)
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

func validate(req Request) (Config, *Response) {
	var cfg Config

	if req.Cmd != "deploy" {
		return cfg, &Response{Error: fmt.Sprintf("unknown command %q", req.Cmd)}
	}

	if !hashRegexp.MatchString(req.Commit) {
		return cfg, &Response{Error: fmt.Sprintf("invalid commit %q", req.Commit)}
	}
	check := exec.Command("git", "--git-dir="+repoPath, "cat-file", "-e", req.Commit+"^{commit}")
	if check.Run() != nil {
		return cfg, &Response{Error: "commit " + req.Commit[:7] + " not in repo, did the push fail?"}
	}

	if strings.TrimSpace(req.Config) == "" {
		return cfg, &Response{Error: "missing .dgett", Hint: configTemplate}
	}
	md, err := toml.Decode(req.Config, &cfg)
	var perr toml.ParseError
	if errors.As(err, &perr) {
		return cfg, &Response{Error: ".dgett: " + perr.ErrorWithPosition()}
	}
	if err != nil {
		return cfg, &Response{Error: ".dgett " + err.Error()}
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		return cfg, &Response{Error: fmt.Sprintf(".dgett: unknown key %q", extra[0].String()), Hint: configTemplate}
	}
	if cfg.Run == "" {
		return cfg, &Response{Error: ".dgett: run is required", Hint: configTemplate}
	}

	return cfg, nil
}

func parsecommand() string {
	if len(os.Args) < 2 {
		return ""
	}
	return os.Args[1]
}

func main() {
	switch cmd := parsecommand(); cmd {
		case "--daemon":
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
					fmt.Fprintf(os.Stderr, "\nexample .dgett:\n%s", resp.Hint)
				}
				os.Exit(1)
			}
			fmt.Println("deployed", dgst[:7])

		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		}
}
