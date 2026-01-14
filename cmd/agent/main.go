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
	"github.com/budhilaw/malangpanel-agent/internal/pki"
)

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	// Parse flags
	configPath := flag.String("config", "/etc/malangpanel/agent.yaml", "Path to configuration file")
	flagToken := flag.String("token", "", "Authentication token (for initial registration)")
	flagID := flag.String("id", "", "Agent ID")
	flagPanel := flag.String("panel", "", "Panel URL (e.g., https://panel.example.com)")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("malangpanel-agent %s (built %s)\n", version, buildTime)
		os.Exit(0)
	}

	// Load configuration (or create default if not exists)
	cfg, err := config.LoadOrCreate(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Override config with flags
	configModified := false
	if *flagToken != "" {
		cfg.Agent.Token = *flagToken
		configModified = true
	}
	if *flagID != "" {
		cfg.Agent.ID = *flagID
		configModified = true
	}

	// PKI paths
	pkiDir := "/etc/malangpanel/pki"
	certPath := pkiDir + "/agent.crt"
	keyPath := pkiDir + "/agent.key"
	caCertPath := pkiDir + "/ca.crt"

	// If panel URL is provided, we need to do certificate registration
	if *flagPanel != "" {
		log.Printf("Panel URL provided, initiating certificate registration...")

		// Create registration client
		client := pki.NewRegistrationClient(*flagPanel)

		// Get agent config from panel (gRPC address and CA cert)
		agentConfig, err := client.GetAgentConfig()
		if err != nil {
			log.Fatalf("Failed to get agent config from panel: %v", err)
		}

		// Save CA certificate
		if agentConfig.CACert != "" {
			if err := pki.SaveCACert(agentConfig.CACert, caCertPath); err != nil {
				log.Fatalf("Failed to save CA certificate: %v", err)
			}
			log.Printf("CA certificate saved to %s", caCertPath)
		}

		// Update control plane address from panel
		if agentConfig.GRPCAddress != "" {
			cfg.ControlPlane.Address = agentConfig.GRPCAddress
			configModified = true
		}

		// Check if we need to register a certificate
		if !pki.CertificateExists(certPath, keyPath) {
			log.Printf("No valid certificate found, generating keypair...")

			// Generate keypair
			keyPair, err := pki.GenerateKeyPair()
			if err != nil {
				log.Fatalf("Failed to generate keypair: %v", err)
			}

			// Save private key
			if err := keyPair.SavePrivateKey(keyPath); err != nil {
				log.Fatalf("Failed to save private key: %v", err)
			}
			log.Printf("Private key saved to %s", keyPath)

			// Generate CSR
			csrPEM, err := keyPair.GenerateCSR(cfg.Agent.ID, cfg.Agent.Name)
			if err != nil {
				log.Fatalf("Failed to generate CSR: %v", err)
			}

			// Register certificate with panel
			log.Printf("Requesting certificate from panel...")
			certPEM, err := client.RegisterCertificate(cfg.Agent.ID, cfg.Agent.Token, csrPEM)
			if err != nil {
				log.Fatalf("Failed to register certificate: %v", err)
			}

			// Save certificate
			if err := pki.SaveCertificate(certPEM, certPath); err != nil {
				log.Fatalf("Failed to save certificate: %v", err)
			}
			log.Printf("Certificate saved to %s", certPath)
		} else {
			log.Printf("Valid certificate found at %s", certPath)
		}

		// Update TLS config
		cfg.TLS.Enabled = true
		cfg.TLS.Cert = certPath
		cfg.TLS.Key = keyPath
		cfg.TLS.CACert = caCertPath
		configModified = true
	}

	// If config was modified, save it
	if configModified {
		if _, err := os.Stat(*configPath); os.IsNotExist(err) {
			log.Printf("Saving configuration to %s", *configPath)
			if err := cfg.Save(*configPath); err != nil {
				log.Printf("Warning: failed to save config: %v", err)
			}
		}
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
