// Command graphify-worker is the NATS-driven graphify worker (spec §11).
//
// Placeholder: Phase 3 will consume cloned events and shell out to the
// `graphify extract` binary (present in this image, which is built FROM the
// graphify runtime) to produce graphify-out artifacts, then publish
// graph-ready events. For now it loads config so the image is real and
// buildable, logs its intent, and exits cleanly.
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
	logger.Warn("graphify-worker is a Phase 3 placeholder — graphify worker not yet implemented",
		"repos_root", cfg.ReposRoot,
	)
}
