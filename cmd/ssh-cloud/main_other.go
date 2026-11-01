//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "ssh-cloud is a Windows application; build it with GOOS=windows")
	os.Exit(1)
}
