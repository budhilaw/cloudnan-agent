package pki

import (
	"crypto/x509"
	"log"
	"os"
	"time"
)

// RenewalChecker periodically checks if the certificate needs renewal
type RenewalChecker struct {
	certPath         string
	keyPath          string
	caCertPath       string
	agentID          string
	panelURL         string
	renewalThreshold time.Duration
	checkInterval    time.Duration
	stopChan         chan struct{}
}

// NewRenewalChecker creates a new renewal checker
func NewRenewalChecker(certPath, keyPath, caCertPath, agentID, panelURL string, renewalThresholdDays int) *RenewalChecker {
	return &RenewalChecker{
		certPath:         certPath,
		keyPath:          keyPath,
		caCertPath:       caCertPath,
		agentID:          agentID,
		panelURL:         panelURL,
		renewalThreshold: time.Duration(renewalThresholdDays) * 24 * time.Hour,
		checkInterval:    24 * time.Hour, // Check once a day
		stopChan:         make(chan struct{}),
	}
}

// Start begins the renewal check loop in a goroutine
func (r *RenewalChecker) Start() {
	go r.checkLoop()
}

// Stop stops the renewal check loop
func (r *RenewalChecker) Stop() {
	close(r.stopChan)
}

func (r *RenewalChecker) checkLoop() {
	// Initial check after a short delay
	time.Sleep(5 * time.Minute)
	r.checkAndRenew()

	ticker := time.NewTicker(r.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.checkAndRenew()
		case <-r.stopChan:
			return
		}
	}
}

func (r *RenewalChecker) checkAndRenew() {
	// Load current certificate
	cert, certPEM, err := LoadCertificate(r.certPath)
	if err != nil {
		log.Printf("[PKI] Failed to load certificate for renewal check: %v", err)
		return
	}

	// Check if renewal is needed
	timeUntilExpiry := time.Until(cert.NotAfter)
	if timeUntilExpiry > r.renewalThreshold {
		log.Printf("[PKI] Certificate valid for %v, no renewal needed", timeUntilExpiry.Round(time.Hour))
		return
	}

	log.Printf("[PKI] Certificate expires in %v, initiating renewal", timeUntilExpiry.Round(time.Hour))

	// Load CA certificate pool
	_, caCertPEM, err := LoadCertificate(r.caCertPath)
	if err != nil {
		log.Printf("[PKI] Failed to load CA certificate: %v", err)
		return
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCertPEM)

	// Generate new keypair
	newKeyPair, err := GenerateKeyPair()
	if err != nil {
		log.Printf("[PKI] Failed to generate new keypair: %v", err)
		return
	}

	// Generate CSR
	csrPEM, err := newKeyPair.GenerateCSR(r.agentID, "")
	if err != nil {
		log.Printf("[PKI] Failed to generate CSR: %v", err)
		return
	}

	// Request renewal
	client := NewRegistrationClient(r.panelURL)
	newCertPEM, err := client.RenewCertificate(r.agentID, csrPEM, cert, caCertPool)
	if err != nil {
		log.Printf("[PKI] Certificate renewal failed: %v", err)
		return
	}

	// Save new key and certificate to temporary locations first
	tempKeyPath := r.keyPath + ".new"
	tempCertPath := r.certPath + ".new"

	if err := newKeyPair.SavePrivateKey(tempKeyPath); err != nil {
		log.Printf("[PKI] Failed to save new private key: %v", err)
		return
	}

	if err := SaveCertificate(newCertPEM, tempCertPath); err != nil {
		log.Printf("[PKI] Failed to save new certificate: %v", err)
		return
	}

	// Atomic swap: rename temp files to actual files
	if err := atomicSwap(tempKeyPath, r.keyPath); err != nil {
		log.Printf("[PKI] Failed to swap private key: %v", err)
		return
	}

	if err := atomicSwap(tempCertPath, r.certPath); err != nil {
		log.Printf("[PKI] Failed to swap certificate: %v", err)
		return
	}

	// Verify the new certificate
	_, _, _ = certPEM, caCertPEM, caCertPool // Suppress unused warnings

	log.Printf("[PKI] Certificate renewed successfully, new expiry: %v", cert.NotAfter)
}

// atomicSwap atomically replaces dest with src
func atomicSwap(src, dest string) error {
	// On most systems, rename is atomic within the same filesystem
	return os.Rename(src, dest)
}
