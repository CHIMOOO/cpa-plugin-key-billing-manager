package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"cpa-key-billing/internal/statecollector"
)

// Set with -ldflags "-X main.version=vX.Y.Z" when packaging the standalone
// collector. This command does not link the embedded plugin or SQLite runtime.
var version = "dev"

func main() {
	managementURL := flag.String("url", "", "CPA HTTP(S) origin (default CPA_URL or http://127.0.0.1:8317)")
	keyFile := flag.String("management-key-file", "", "Read the management key from this file before each request (otherwise CPA_MANAGEMENT_KEY)")
	check := flag.Bool("check", false, "Verify the read-only runner endpoint and exit without enabling or probing")
	showVersion := flag.Bool("version", false, "Print collector version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	logger := log.New(os.Stderr, "cpa-state-collector: ", log.LstdFlags|log.LUTC)
	if flag.NArg() != 0 {
		logger.Print("unexpected positional arguments; see --help")
		os.Exit(2)
	}
	if strings.TrimSpace(*managementURL) == "" {
		*managementURL = strings.TrimSpace(os.Getenv("CPA_URL"))
	}
	collector, err := statecollector.New(statecollector.Config{URL: *managementURL, ManagementKeyFile: *keyFile, Version: version, Logger: logger})
	if err != nil {
		logger.Print(err)
		os.Exit(2)
	}
	defer collector.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *check {
		status, err := collector.Check(ctx)
		if err != nil {
			logger.Print(err)
			os.Exit(1)
		}
		logger.Printf("management endpoint is reachable; collection enabled=%t phase=%s", status.Enabled, status.Phase)
		return
	}
	logger.Print("collector started; the plugin controls whether collection is enabled")
	if err := collector.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Print("collector stopped unexpectedly")
		os.Exit(1)
	}
	logger.Print("collector stopped")
}
