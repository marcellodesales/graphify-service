// Command graphify-cloner is the NATS-driven clone worker (spec §9).
//
// Placeholder: Phase 2 will consume clone-requested events, perform secure
// shallow/SHA-only clones into the shared volume, and publish cloned events.
// For now it loads config so the image is real and buildable, logs its intent,
// and exits cleanly.
package main

import (
	"fmt"
	"os"

	"github.com/marcellodesales/graphify-service/backend/internal/config"
	"github.com/marcellodesales/graphify-service/backend/internal/telemetry"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	logger := telemetry.NewLogger(cfg.LogLevel)
	logger.Warn("graphify-cloner is a Phase 2 placeholder — clone worker not yet implemented",
		"repos_root", cfg.ReposRoot,
	)
}
