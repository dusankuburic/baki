// PAD Analyzer backend — local (Tauri sidecar) and cloud (multi-tenant) modes.
//
//	@title			PAD Analyzer API
//	@version		1.0
//	@description	Static analysis, auto-fix, and AI-assisted review for Power Automate Desktop flows.
//
//	@BasePath	/
package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
	// The runtime image is distroless: without the embedded zone database
	// time.LoadLocation knows only UTC, and the analytics tz parameter fails.
	_ "time/tzdata"

	"github.com/google/uuid"
	"go.uber.org/fx"

	"pad-analyzer/internal/app"
	"pad-analyzer/internal/config"
)

var (
	Version   = "0.1.0"
	BuildDate = ""
	GitCommit = ""
)

func main() {
	// The parser mints one UUID per block and the apply-fix loop re-parses
	// per fix; batched randomness (still crypto-seeded) avoids a syscall per block.
	uuid.EnableRandPool()

	// `baki-backend healthcheck` GETs the server's own /readyz (exit 0 on 200)
	// so non-ACA deployments (compose, bare docker run) can probe without a
	// shell/wget — which the distroless image doesn't have. ACA uses platform
	// httpGet probes instead.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	fx.New(appOptions()...).Run()
}

// appOptions is the application graph with this binary's build stamps: main runs it, TestAppGraph validates it without
// running anything. The graph itself is internal/app's.
func appOptions() []fx.Option {
	return app.Options(app.Build{Version: Version, Commit: GitCommit})
}

// runHealthcheck implements the `baki-backend healthcheck` subcommand: GET the
// server's own /readyz, 0 on 200, 1 otherwise. Used by docker-compose and
// bare-docker operators because the distroless image has no shell/wget.
// Bounded by a short timeout so a hung server is reported unhealthy.
func runHealthcheck() int {
	port, err := config.HealthcheckPort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second}
	// #nosec G704 -- not attacker-controlled: a fixed loopback host and a
	// constant path, with only the port coming from this process's own
	// configuration. This is the container healthcheck probing its own server.
	resp, err := client.Get("http://localhost:" + strconv.Itoa(port) + "/readyz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}
