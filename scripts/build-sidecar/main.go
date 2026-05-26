package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func main() {
	// Get target triple
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

	// Create bin directory
	err = os.MkdirAll("src-tauri/bin", 0755)
	if err != nil {
		fmt.Printf("Error creating bin directory: %v\n", err)
		os.Exit(1)
	}

	// Build Go binary
	exeName := "pad-backend-" + triple
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}

	fmt.Printf("Building sidecar: %s\n", exeName)
	cmd := exec.Command("go", "build", "-o", "src-tauri/bin/"+exeName, ".")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		fmt.Printf("Error building Go binary: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Sidecar built successfully")
}
