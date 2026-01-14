package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// RegistrationClient handles certificate registration with the panel
type RegistrationClient struct {
	panelURL   string
	httpClient *http.Client
}

// AgentConfig represents the configuration returned by the panel
type AgentConfig struct {
	GRPCAddress string `json:"grpc_address"`
	CACert      string `json:"ca_cert"`
}

// CertificateResponse represents the response from certificate registration
type CertificateResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Certificate string `json:"certificate"`
	} `json:"data"`
}

// NewRegistrationClient creates a new registration client
func NewRegistrationClient(panelURL string) *RegistrationClient {
	return &RegistrationClient{
		panelURL: panelURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // For initial registration before we have CA cert
				},
			},
		},
	}
}

// GetAgentConfig fetches the agent configuration from the panel
func (c *RegistrationClient) GetAgentConfig() (*AgentConfig, error) {
	resp, err := c.httpClient.Get(c.panelURL + "/api/agent-config")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch agent config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to fetch agent config: status %d, body: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Success bool        `json:"success"`
		Data    AgentConfig `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode agent config: %w", err)
	}

	if !result.Success {
		return nil, fmt.Errorf("failed to get agent config")
	}

	return &result.Data, nil
}

// RegisterCertificate sends a CSR to the panel and receives a signed certificate
func (c *RegistrationClient) RegisterCertificate(agentID, token string, csrPEM []byte) ([]byte, error) {
	payload := map[string]string{
		"csr":   string(csrPEM),
		"token": token,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/agents/%s/certificate", c.panelURL, agentID)
	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to register certificate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to register certificate: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var result CertificateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if !result.Success {
		return nil, fmt.Errorf("certificate registration failed: %s", result.Message)
	}

	return []byte(result.Data.Certificate), nil
}

// RenewCertificate renews an existing certificate
func (c *RegistrationClient) RenewCertificate(agentID string, csrPEM []byte, currentCert *x509.Certificate, caCertPool *x509.CertPool) ([]byte, error) {
	// Create client with mTLS for renewal
	// Note: For renewal, we use the existing certificate to authenticate
	payload := map[string]string{
		"csr": string(csrPEM),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/agents/%s/renew-certificate", c.panelURL, agentID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Cert-Fingerprint", CertificateFingerprint(currentCert))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to renew certificate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to renew certificate: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	var result CertificateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if !result.Success {
		return nil, fmt.Errorf("certificate renewal failed: %s", result.Message)
	}

	return []byte(result.Data.Certificate), nil
}

// SaveCACert saves the CA certificate to a file
func SaveCACert(caCertPEM string, path string) error {
	return SaveCertificate([]byte(caCertPEM), path)
}
