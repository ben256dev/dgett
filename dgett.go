package main



import (
	"errors"
	"fmt"
	"io"
	_ "log"
	"os"
	"os/exec"
	"time"
)

func main() {
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
}
