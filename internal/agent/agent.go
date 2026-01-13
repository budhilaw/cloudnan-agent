package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/budhilaw/malangpanel-agent/internal/config"
	"github.com/budhilaw/malangpanel-agent/internal/executor"
	"github.com/budhilaw/malangpanel-agent/internal/monitor"
	"github.com/budhilaw/malangpanel-agent/internal/ssh"
	pb "github.com/budhilaw/malangpanel-agent/proto/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Agent is the main agent struct
type Agent struct {
	cfg        *config.Config
	executor   *executor.Executor
	monitor    *monitor.Monitor
	sshHandler *ssh.Handler
	conn       *grpc.ClientConn

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
	sshHandler := ssh.NewHandler("/var/backups/malangpanel/ssh")

	return &Agent{
		cfg:        cfg,
		executor:   exec,
		monitor:    mon,
		sshHandler: sshHandler,
		agentID:    agentID,
		hostname:   hostname,
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
			_ = a.conn.Close()
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

	// Connect using NewClient (replaces deprecated DialContext)
	conn, err := grpc.NewClient(a.cfg.ControlPlane.Address, opts...)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	a.conn = conn

	log.Println("Connected to Control Plane")

	// Create gRPC client
	client := pb.NewAgentServiceClient(conn)

	// Add auth metadata
	ctxWithAuth := ctx
	if a.cfg.Agent.Token != "" {
		ctxWithAuth = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.cfg.Agent.Token)
	}

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

	regResp, err := client.Register(ctxWithAuth, regReq)
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

	go a.runCommandStream(commandCtx, client, a.cfg.Agent.Token)
	go a.runMetricsStream(commandCtx, client, a.cfg.Agent.Token)

	// Heartbeat loop
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeatTicker.C:
			_, err := client.Heartbeat(ctxWithAuth, &pb.HeartbeatRequest{
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
func (a *Agent) runCommandStream(ctx context.Context, client pb.AgentServiceClient, token string) {
	log.Println("Starting command stream...")

	// Add auth metadata
	ctxWithAuth := ctx
	if token != "" {
		ctxWithAuth = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}

	stream, err := client.CommandStream(ctxWithAuth)
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

// runMetricsStream handles metrics streaming
func (a *Agent) runMetricsStream(ctx context.Context, client pb.AgentServiceClient, token string) {
	log.Println("Starting metrics stream...")

	// Add auth metadata
	ctxWithAuth := ctx
	if token != "" {
		ctxWithAuth = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}

	stream, err := client.StreamMetrics(ctxWithAuth)
	if err != nil {
		log.Printf("Failed to open metrics stream: %v", err)
		return
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Get system info using monitor
			metrics, err := a.monitor.Collect()
			if err != nil {
				log.Printf("Failed to collect metrics: %v", err)
				continue
			}

			// Convert disk metrics with filtering AND fstype
			var diskMetrics []*pb.DiskMetrics
			for _, d := range metrics.Disks {
				// Filter out insignificant mounts (heuristic)
				// Skip if total size is very small (< 10MB) - likely not a real disk
				if d.Total < 10*1024*1024 {
					continue
				}

				diskMetrics = append(diskMetrics, &pb.DiskMetrics{
					MountPoint:    d.MountPoint,
					Device:        d.Device,
					Total:         d.Total,
					Used:          d.Used,
					Free:          d.Free,
					Percent:       d.Percent,
					Fstype:        d.Fstype,
					InodesTotal:   d.InodesTotal,
					InodesUsed:    d.InodesUsed,
					InodesPercent: d.InodesPercent,
				})
			}

			// Convert Disk I/O
			var diskIO []*pb.DiskIO
			for _, io := range metrics.DiskIO {
				diskIO = append(diskIO, &pb.DiskIO{
					Name:       io.Name,
					ReadCount:  io.ReadCount,
					WriteCount: io.WriteCount,
					ReadBytes:  io.ReadBytes,
					WriteBytes: io.WriteBytes,
				})
			}

			// Host Info
			var hostInfo *pb.HostInfo
			if metrics.HostInfo != nil {
				hostInfo = &pb.HostInfo{
					Hostname:             metrics.HostInfo.Platform, // Misnomer in gopsutil?
					Os:                   metrics.HostInfo.Platform, // Use Platform as OS name
					Platform:             metrics.HostInfo.Platform,
					PlatformFamily:       metrics.HostInfo.PlatformFamily,
					PlatformVersion:      metrics.HostInfo.PlatformVersion,
					KernelVersion:        metrics.HostInfo.KernelVersion,
					KernelArch:           metrics.HostInfo.KernelArch,
					VirtualizationSystem: metrics.HostInfo.VirtualizationSystem,
					VirtualizationRole:   metrics.HostInfo.VirtualizationRole,
					BootTime:             metrics.HostInfo.BootTime,
				}
			}

			// Get Services
			var pbServices []*pb.ServiceInfo
			services := a.monitor.GetServices()
			for _, s := range services {
				pbServices = append(pbServices, &pb.ServiceInfo{
					Name:        s.Name,
					Status:      s.Status,
					Restarts:    s.Restarts,
					LastRestart: s.LastRestart,
				})
			}

			// Get Containers
			var pbContainers []*pb.ContainerInfo
			containers := a.monitor.GetContainers()
			for _, c := range containers {
				pbContainers = append(pbContainers, &pb.ContainerInfo{
					Id:        c.ID,
					Name:      c.Name,
					Image:     c.Image,
					Status:    c.Status,
					Health:    c.Health,
					Restarts:  c.Restarts,
					StartedAt: c.StartedAt,
				})
			}

			// Get Firewall
			fwInfo := a.monitor.GetFirewall()
			var pbRules []*pb.FirewallRule
			for _, r := range fwInfo.Rules {
				pbRules = append(pbRules, &pb.FirewallRule{
					Index:    r.Index,
					To:       r.To,
					Action:   r.Action,
					From:     r.From,
					Protocol: r.Protocol,
					Comment:  r.Comment,
				})
			}
			pbFirewall := &pb.FirewallInfo{
				Status: fwInfo.Status,
				Rules:  pbRules,
			}

			pbMetrics := &pb.MetricsData{
				AgentId:          a.agentID,
				Timestamp:        time.Now().Unix(),
				CpuPercent:       metrics.CPUPercent,
				CpuPerCore:       metrics.CPUPerCore,
				MemoryTotal:      metrics.MemoryTotal,
				MemoryUsed:       metrics.MemoryUsed,
				MemoryAvailable:  metrics.MemoryAvailable,
				MemoryPercent:    metrics.MemoryPercent,
				MemoryCached:     metrics.MemoryCached,
				MemoryBuffers:    metrics.MemoryBuffers,
				SwapTotal:        metrics.SwapTotal,
				SwapUsed:         metrics.SwapUsed,
				SwapFree:         metrics.SwapFree,
				SwapPercent:      metrics.SwapPercent,
				Disks:            diskMetrics,
				DiskIo:           diskIO,
				NetworkBytesSent: metrics.NetworkBytesSent,
				NetworkBytesRecv: metrics.NetworkBytesRecv,
				Connections:      metrics.Connections,
				Load_1:           metrics.Load1,
				Load_5:           metrics.Load5,
				Load_15:          metrics.Load15,
				UptimeSeconds:    metrics.UptimeSeconds,
				HostInfo:         hostInfo,
				Services:         pbServices,
				Containers:       pbContainers,
				Firewall:         pbFirewall,
			}

			if err := stream.Send(pbMetrics); err != nil {
				log.Printf("Failed to send metrics: %v", err)
				return
			}
		}
	}
}

// executeCommand runs a command and sends the response
func (a *Agent) executeCommand(ctx context.Context, stream pb.AgentService_CommandStreamClient, cmd *pb.Command) {
	// Send running status
	_ = stream.Send(&pb.CommandResponse{
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

	case pb.CommandType_COMMAND_TYPE_DOCKER:
		// Execute docker command
		// e.g. args=["ps", "-a"] -> "docker ps -a"
		result, execErr = a.executor.Execute(execCtx, "docker", cmd.Args, nil, timeout)

	case pb.CommandType_COMMAND_TYPE_SERVICE:
		// Execute systemctl command
		// e.g. args=["status", "nginx"] -> "systemctl status nginx"
		result, execErr = a.executor.Execute(execCtx, "systemctl", cmd.Args, nil, timeout)

	case pb.CommandType_COMMAND_TYPE_INSTALL:
		// Try to detect package manager (simple heuristic)
		// This is a naive implementation, real world would need better OS detection
		pkgManager := "apt-get"
		if _, err := os.Stat("/usr/bin/yum"); err == nil {
			pkgManager = "yum"
		} else if _, err := os.Stat("/usr/bin/dnf"); err == nil {
			pkgManager = "dnf"
		} else if _, err := os.Stat("/sbin/apk"); err == nil {
			pkgManager = "apk"
		}

		// Prepend non-interactive flags if needed
		args := cmd.Args
		switch pkgManager {
		case "apt-get":
			args = append([]string{"-y"}, args...)
		case "yum", "dnf":
			args = append([]string{"-y"}, args...)
		}

		result, execErr = a.executor.Execute(execCtx, pkgManager, args, nil, timeout)

	case pb.CommandType_COMMAND_TYPE_FILE:
		// args[0] = operation (cat, ls, rm)
		// args[1] = path
		if len(cmd.Args) < 2 {
			execErr = fmt.Errorf("file command requires operation and path")
			break
		}
		op := cmd.Args[0]
		path := cmd.Args[1]

		switch op {
		case "cat", "read":
			result, execErr = a.executor.Execute(execCtx, "cat", []string{path}, nil, timeout)
		case "ls", "list":
			result, execErr = a.executor.Execute(execCtx, "ls", []string{"-la", path}, nil, timeout)
		case "rm", "delete":
			result, execErr = a.executor.Execute(execCtx, "rm", []string{"-rf", path}, nil, timeout)
		default:
			execErr = fmt.Errorf("unknown file operation: %s", op)
		}

	case pb.CommandType_COMMAND_TYPE_SSH:
		// SSH operations
		// args[0] = operation (sync_keys, update_config, get_status, restart)
		if len(cmd.Args) < 1 {
			execErr = fmt.Errorf("SSH command requires operation")
			break
		}
		op := cmd.Args[0]
		result = a.executeSSHCommand(op, cmd.Args[1:])

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

// executeSSHCommand handles SSH-specific operations
func (a *Agent) executeSSHCommand(operation string, args []string) *executor.Result {
	result := &executor.Result{
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	}

	switch operation {
	case "sync_keys":
		// args[0] = JSON encoded keys array
		// args[1] = target user (optional)
		if len(args) < 1 {
			result.ExitCode = 1
			result.Stderr = "sync_keys requires keys JSON as argument"
			return result
		}

		var keys []ssh.KeyEntry
		if err := json.Unmarshal([]byte(args[0]), &keys); err != nil {
			result.ExitCode = 1
			result.Stderr = fmt.Sprintf("failed to parse keys JSON: %v", err)
			return result
		}

		targetUser := "root"
		if len(args) > 1 && args[1] != "" {
			targetUser = args[1]
		}

		resp := a.sshHandler.SyncKeys(&ssh.SyncKeysRequest{
			Keys:       keys,
			TargetUser: targetUser,
			ReplaceAll: true,
		})

		if resp.Success {
			result.Stdout = fmt.Sprintf("Successfully synced %d keys. Backup: %s", resp.KeysSynced, resp.BackupPath)
		} else {
			result.ExitCode = 1
			result.Stderr = resp.Message
		}

	case "update_config":
		// args[0] = JSON encoded config update
		if len(args) < 1 {
			result.ExitCode = 1
			result.Stderr = "update_config requires config JSON as argument"
			return result
		}

		var req ssh.ConfigUpdateRequest
		if err := json.Unmarshal([]byte(args[0]), &req); err != nil {
			result.ExitCode = 1
			result.Stderr = fmt.Sprintf("failed to parse config JSON: %v", err)
			return result
		}

		resp := a.sshHandler.UpdateConfig(&req)
		if resp.Success {
			msg := "SSH configuration updated successfully"
			if resp.SSHDRestarted {
				msg += " (sshd restarted)"
			}
			if resp.BackupPath != "" {
				msg += fmt.Sprintf(". Backup: %s", resp.BackupPath)
			}
			result.Stdout = msg
		} else {
			result.ExitCode = 1
			result.Stderr = resp.Message
		}

	case "get_status":
		// args[0] = target user (optional)
		targetUser := "root"
		if len(args) > 0 && args[0] != "" {
			targetUser = args[0]
		}

		status, err := a.sshHandler.GetStatus(targetUser)
		if err != nil {
			result.ExitCode = 1
			result.Stderr = fmt.Sprintf("failed to get SSH status: %v", err)
			return result
		}

		statusJSON, _ := json.Marshal(status)
		result.Stdout = string(statusJSON)

	case "restart":
		if err := a.sshHandler.RestartSSHD(); err != nil {
			result.ExitCode = 1
			result.Stderr = fmt.Sprintf("failed to restart sshd: %v", err)
			return result
		}
		result.Stdout = "sshd restarted successfully"

	case "remove_key":
		// args[0] = key ID
		// args[1] = target user (optional)
		if len(args) < 1 {
			result.ExitCode = 1
			result.Stderr = "remove_key requires key ID as argument"
			return result
		}

		targetUser := "root"
		if len(args) > 1 && args[1] != "" {
			targetUser = args[1]
		}

		if err := a.sshHandler.RemoveKey(args[0], targetUser); err != nil {
			result.ExitCode = 1
			result.Stderr = fmt.Sprintf("failed to remove key: %v", err)
			return result
		}
		result.Stdout = fmt.Sprintf("Key %s removed successfully", args[0])

	default:
		result.ExitCode = 1
		result.Stderr = fmt.Sprintf("unknown SSH operation: %s", operation)
	}

	result.FinishedAt = time.Now()
	return result
}
