package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"pad-analyzer/internal/api"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/manager"

	"github.com/google/uuid"
)

func main() {
	// Initialize App Manager
	app := manager.NewApp()
	
	// Create a security token
	token := uuid.NewString()

	// Initialize the Router (which also acts as a Notifier)
	router := api.NewRouter(app, token)

	// Init the App with the router as notifier
	ctx := context.Background()
	app.Init(ctx, router)

	// Listen on an ephemeral port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen: %v\n", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	// Output port and token for the Tauri host to read
	startupInfo := map[string]interface{}{
		"port":  port,
		"token": token,
	}
	infoJSON, _ := json.Marshal(startupInfo)
	fmt.Println(string(infoJSON))

	// Start the HTTP server
	server := &http.Server{
		Handler: router,
	}

	logger.Info("backend server starting", "port", port)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
