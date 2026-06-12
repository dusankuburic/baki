package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	out, err := exec.Command("rustc", "-vV").Output()
	if err != nil {
		fmt.Printf("Error getting rustc version: %v\n", err)
		os.Exit(1)
	}

	var triple string
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "host:") {
			triple = strings.TrimSpace(strings.TrimPrefix(line, "host:"))
			break
		}
	}

	if triple == "" {
		fmt.Println("Could not determine target triple")
		os.Exit(1)
	}

	binDir := "src-tauri/bin"
	err = os.MkdirAll(binDir, 0755)
	if err != nil {
		fmt.Printf("Error creating bin directory: %v\n", err)
		os.Exit(1)
	}

	exeName := "pad-backend-" + triple
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}

	pattern := filepath.Join(binDir, "pad-backend-*")
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		if filepath.Base(m) != exeName {
			os.Remove(m)
		}
	}

	fmt.Printf("Building sidecar: %s\n", exeName)
	cmd := exec.Command("go", "build",
		"-trimpath",
		"-ldflags=-s -w",
		"-o", filepath.Join(binDir, exeName),
		".")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		fmt.Printf("Error building Go binary: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Sidecar built successfully")
}
