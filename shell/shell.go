package shell

import (
	"os"
	"os/exec"
)

func Pipe(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}
