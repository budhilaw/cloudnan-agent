package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/budhilaw/malangpanel-agent/internal/config"
	"github.com/budhilaw/malangpanel-agent/internal/executor"
	"github.com/budhilaw/malangpanel-agent/internal/monitor"
	pb "github.com/budhilaw/malangpanel-agent/proto/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Agent is the main agent struct
type Agent struct {
	cfg      *config.Config
	executor *executor.Executor
	monitor  *monitor.Monitor
	conn     *grpc.ClientConn

	// Agent state
	agentID  string
	hostname string

	mu      sync.RWMutex
	running bool
}

// New creates a new Agent
func New(cfg *config.Config) (*Agent, error) {
	// Get hostname
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	// Generate agent ID if not set
	agentID := cfg.Agent.ID
	if agentID == "" {
		agentID = fmt.Sprintf("agent-%s-%d", hostname, time.Now().Unix())
	}

	exec := executor.New(
		cfg.Executor.Shell,
		cfg.Executor.DefaultTimeout,
		cfg.Executor.AllowedCommands,
		cfg.Executor.BlockedCommands,
	)

	mon := monitor.New()

	return &Agent{
		cfg:      cfg,
		executor: exec,
		monitor:  mon,
		agentID:  agentID,
		hostname: hostname,
	}, nil
}

// Run starts the agent and connects to Control Plane
func (a *Agent) Run(ctx context.Context) error {
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()

	// Connect to Control Plane
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if err := a.connect(ctx); err != nil {
			log.Printf("Connection failed: %v", err)
			log.Printf("Reconnecting in %v...", a.cfg.ControlPlane.ReconnectInterval)

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(a.cfg.ControlPlane.ReconnectInterval):
				continue
			}
		}

		// Connection closed, reconnect
		if a.conn != nil {
			a.conn.Close()
			a.conn = nil
		}

		log.Println("Connection closed, reconnecting...")
		time.Sleep(a.cfg.ControlPlane.ReconnectInterval)
	}
}

func (a *Agent) connect(ctx context.Context) error {
	log.Printf("Connecting to Control Plane: %s", a.cfg.ControlPlane.Address)

	// Setup credentials
	var opts []grpc.DialOption

	if a.cfg.TLS.Enabled {
		tlsConfig, err := a.loadTLSConfig()
		if err != nil {
			return fmt.Errorf("failed to load TLS config: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		log.Println("WARNING: TLS is disabled, using insecure connection")
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Connect
	conn, err := grpc.DialContext(ctx, a.cfg.ControlPlane.Address, opts...)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	a.conn = conn

	log.Println("Connected to Control Plane")

	// Create gRPC client
	client := pb.NewAgentServiceClient(conn)

	// Get system info for registration
	sysInfo, _ := a.monitor.GetSystemInfo()
	osInfo := "linux"
	osVersion := ""
	arch := runtime.GOARCH
	if sysInfo != nil {
		osInfo = sysInfo.OS
		osVersion = sysInfo.OSVersion
		arch = sysInfo.Arch
	}

	// Register with Control Plane
	regReq := &pb.RegisterRequest{
		AgentId:      a.agentID,
		Hostname:     a.hostname,
		Os:           osInfo,
		OsVersion:    osVersion,
		Arch:         arch,
		AgentVersion: "dev",
		Labels:       a.cfg.Agent.Labels,
	}

	regResp, err := client.Register(ctx, regReq)
	if err != nil {
		return fmt.Errorf("failed to register: %w", err)
	}

	if !regResp.Success {
		return fmt.Errorf("registration failed: %s", regResp.Message)
	}

	log.Printf("Registered with Control Plane: %s (heartbeat: %ds)", regResp.Message, regResp.HeartbeatInterval)

	// Start heartbeat interval
	heartbeatInterval := time.Duration(regResp.HeartbeatInterval) * time.Second
	if heartbeatInterval == 0 {
		heartbeatInterval = 30 * time.Second
	}

	// Start command stream in goroutine
	commandCtx, cancelCommands := context.WithCancel(ctx)
	defer cancelCommands()

	go a.runCommandStream(commandCtx, client)

	// Heartbeat loop
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeatTicker.C:
			_, err := client.Heartbeat(ctx, &pb.HeartbeatRequest{
				AgentId:   a.agentID,
				Timestamp: time.Now().Unix(),
			})
			if err != nil {
				log.Printf("Heartbeat failed: %v", err)
				return err
			}
			log.Println("Heartbeat sent")
		}
	}
}

// runCommandStream handles bidirectional command streaming
func (a *Agent) runCommandStream(ctx context.Context, client pb.AgentServiceClient) {
	log.Println("Starting command stream...")

	stream, err := client.CommandStream(ctx)
	if err != nil {
		log.Printf("Failed to open command stream: %v", err)
		return
	}

	// Send initial identification message
	err = stream.Send(&pb.CommandResponse{
		CommandId: a.agentID, // Use commandId to pass agentID initially
		Status:    pb.CommandStatus_COMMAND_STATUS_PENDING,
	})
	if err != nil {
		log.Printf("Failed to identify on command stream: %v", err)
		return
	}

	log.Println("Command stream connected, waiting for commands...")

	// Receive and execute commands
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		cmd, err := stream.Recv()
		if err != nil {
			log.Printf("Command stream error: %v", err)
			return
		}

		log.Printf("Received command: id=%s type=%v args=%v", cmd.Id, cmd.Type, cmd.Args)

		// Execute command in goroutine
		go a.executeCommand(ctx, stream, cmd)
	}
}

// executeCommand runs a command and sends the response
func (a *Agent) executeCommand(ctx context.Context, stream pb.AgentService_CommandStreamClient, cmd *pb.Command) {
	// Send running status
	stream.Send(&pb.CommandResponse{
		CommandId: cmd.Id,
		Status:    pb.CommandStatus_COMMAND_STATUS_RUNNING,
	})

	// Execute based on type
	var result *executor.Result
	var execErr error

	timeout := time.Duration(cmd.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch cmd.Type {
	case pb.CommandType_COMMAND_TYPE_EXEC:
		// Join args into command string
		cmdStr := ""
		if len(cmd.Args) > 0 {
			cmdStr = cmd.Args[0]
			for i := 1; i < len(cmd.Args); i++ {
				cmdStr += " " + cmd.Args[i]
			}
		}
		result, execErr = a.executor.Execute(execCtx, cmdStr, nil, nil, timeout)

	default:
		result = &executor.Result{
			ExitCode: 1,
			Stderr:   fmt.Sprintf("Unknown command type: %v", cmd.Type),
		}
	}

	// Prepare response
	resp := &pb.CommandResponse{
		CommandId: cmd.Id,
		Status:    pb.CommandStatus_COMMAND_STATUS_COMPLETED,
		ExitCode:  0,
	}

	if execErr != nil {
		resp.Status = pb.CommandStatus_COMMAND_STATUS_FAILED
		resp.Stderr = execErr.Error()
		resp.ExitCode = 1
	} else if result != nil {
		resp.Stdout = result.Stdout
		resp.Stderr = result.Stderr
		resp.ExitCode = int32(result.ExitCode)
		if result.ExitCode != 0 {
			resp.Status = pb.CommandStatus_COMMAND_STATUS_FAILED
		}
	}

	log.Printf("Command %s completed: status=%v exit=%d", cmd.Id, resp.Status, resp.ExitCode)

	if err := stream.Send(resp); err != nil {
		log.Printf("Failed to send command response: %v", err)
	}
}

func (a *Agent) loadTLSConfig() (*tls.Config, error) {
	// Load CA certificate
	caCert, err := os.ReadFile(a.cfg.TLS.CACert)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA cert: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA cert")
	}

	// Load client certificate
	clientCert, err := tls.LoadX509KeyPair(a.cfg.TLS.Cert, a.cfg.TLS.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to load client cert: %w", err)
	}

	return &tls.Config{
		Certificates:       []tls.Certificate{clientCert},
		RootCAs:            caCertPool,
		InsecureSkipVerify: a.cfg.TLS.InsecureSkipVerify,
	}, nil
}

// GetID returns the agent ID
func (a *Agent) GetID() string {
	return a.agentID
}

// GetHostname returns the hostname
func (a *Agent) GetHostname() string {
	return a.hostname
}

// IsRunning returns whether the agent is running
func (a *Agent) IsRunning() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.running
}
