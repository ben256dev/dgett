package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	_ "io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	_ "time"
)

const sockPath = "/run/dgett/dgett.sock"
const repoPath = "/var/lib/dgett/repo.git"

func handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintf(conn, "got: %s\n", line)
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
	return git(nil, "commit-tree", tree, "-p", "HEAD", "-m", "dgett snapshot")
}

func push(commit string) error {
	_, err := git(nil, "push", "--quiet", repoPath, commit+":refs/dgett/"+commit)
	return err
}

func main() {
	if len(os.Args) == 2 {
		if os.Args[1] == "--daemon" {
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
			for {
				conn, err := ln.Accept()
				if err != nil {
					log.Fatalf("failed to Accept", sockPath)
				}
				go handle(conn)
			}
		} else {
			log.Printf("unknown command: %s\n", os.Args[1])
		}
	} else {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			log.Fatal("daemon not running: ", err)
		}
		defer conn.Close()

		dgst, err := snapshot()
		if err != nil {
			log.Fatal(err)
		}

		push(dgst)

		type Request struct {
			Cmd string `json:"cmd"`
			Dgst string `json:"dgst"`
			Config string `json:"config"`
		}

		top, err := git(nil, "rev-parse", "--show-toplevel")

		data, err := os.ReadFile(filepath.Join(top, ".dgett"))
		if err != nil {
			log.Fatal("no .dgett file")
		}

		json.NewEncoder(conn).Encode(Request{
			Cmd: "deploy",
			Dgst: dgst,
			Config: string(data),
		})

		reply, _ := bufio.NewReader(conn).ReadString('\n')
		fmt.Print(reply)
	}

	/*
	name := fmt.Sprintf("%s-%s.log", "ls", time.Now().Format("20060102-150405"))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	lsCmd := exec.Command("ls")
	lsCmd.Stdin = os.Stdin
	lsCmd.Stdout = io.MultiWriter(os.Stdout, f)
	lsCmd.Stderr = io.MultiWriter(os.Stderr, f)

	err = lsCmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		f.Close()
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	*/
}
