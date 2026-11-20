package main

import (
	"bufio"
	"errors"
	"fmt"
	_ "io"
	"io/fs"
	"log"
	"net"
	"os"
	_ "os/exec"
	"os/user"
	"strconv"
	_ "time"
)

const sockPath = "/run/dgett/dgett.sock"

func handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintf(conn, "got: %s\n", line)
	}
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
		fmt.Fprintln(conn, "hello")
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
