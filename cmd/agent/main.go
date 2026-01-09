package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/budhilaw/malangpanel-agent/internal/agent"
	"github.com/budhilaw/malangpanel-agent/internal/config"
)

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	// Parse flags
	configPath := flag.String("config", "/etc/malangpanel/agent.yaml", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("malangpanel-agent %s (built %s)\n", version, buildTime)
		os.Exit(0)
	}

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Setup logging
	setupLogging(cfg.Logging)

	log.Printf("Starting Malang Panel Agent %s", version)
	log.Printf("Control Plane: %s", cfg.ControlPlane.Address)

	// Create agent
	ag, err := agent.New(cfg)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Run agent
	if err := ag.Run(ctx); err != nil {
		log.Fatalf("Agent error: %v", err)
	}

	log.Println("Agent stopped")
}

func setupLogging(cfg config.LoggingConfig) {
	// For now, just use standard log
	// In production, use zerolog or zap
	if cfg.File != "" {
		f, err := os.OpenFile(cfg.File, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			log.Printf("Warning: couldn't open log file %s: %v", cfg.File, err)
			return
		}
		log.SetOutput(f)
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Add timestamp prefix
	_ = time.Now() // placeholder for custom log format
}
