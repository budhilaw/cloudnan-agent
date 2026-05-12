package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudnan-tech/cloudnan-agent/internal/config"
	"github.com/cloudnan-tech/cloudnan-agent/internal/database"
	"github.com/cloudnan-tech/cloudnan-agent/internal/executor"
	"github.com/cloudnan-tech/cloudnan-agent/internal/filesystem"
	"github.com/cloudnan-tech/cloudnan-agent/internal/monitor"
	"github.com/cloudnan-tech/cloudnan-agent/internal/ssh"
	pb "github.com/cloudnan-tech/cloudnan-agent/proto/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

// Agent is the main agent struct
type Agent struct {
	cfg        *config.Config
	executor   *executor.Executor
	monitor    *monitor.Monitor
	sshHandler *ssh.Handler
	fsManager  *filesystem.Manager
	dbHandler  *database.Handler
	conn       *grpc.ClientConn

	// Agent state
	agentID  string
	hostname string

	mu      sync.RWMutex
	running bool
	version string

	// fastReconnect is set when the server requests an immediate reconnect
	// (e.g. during a blue-green deploy). The reconnect loop uses 1s retry
	// intervals for 30 attempts before falling back to normal backoff.
	fastReconnect atomic.Bool

	// reconnectCh signals the heartbeat loop to exit immediately and trigger
	// a fast reconnect. Written by executeCommand on COMMAND_TYPE_RECONNECT.
	reconnectCh chan struct{}
}

// New creates a new Agent
func New(cfg *config.Config, version string) (*Agent, error) {
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

	// Bind the agent_id into the destructive-op token verifier so it can
	// reject tokens minted for a different agent (cross-agent replay defense).
	database.SetAgentID(agentID)

	exec := executor.New(
		cfg.Executor.Shell,
		cfg.Executor.DefaultTimeout,
		cfg.Executor.AllowedCommands,
		cfg.Executor.BlockedCommands,
	)

	mon := monitor.New()
	sshHandler := ssh.NewHandler("/var/backups/cloudnan/ssh")
	fsManager := filesystem.New("/")
	dbHandler := database.NewHandler()

	// Start the AI backup janitor — prunes /var/backups/cloudnan
	// entries older than 30 days, evicts oldest first when the total
	// exceeds 1 GiB. Runs in its own goroutine for the lifetime of
	// the agent process.
	filesystem.StartBackupJanitor("")

	return &Agent{
		cfg:         cfg,
		executor:    exec,
		monitor:     mon,
		sshHandler:  sshHandler,
		fsManager:   fsManager,
		dbHandler:   dbHandler,
		agentID:     agentID,
		hostname:    hostname,
		version:     version,
		reconnectCh: make(chan struct{}, 1),
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

	backoff := 5 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return nil
		}

		err := a.connect(ctx)

		// Close old connection before potentially reconnecting
		if a.conn != nil {
			_ = a.conn.Close()
			a.conn = nil
		}

		if ctx.Err() != nil {
			return nil
		}

		if a.fastReconnect.Swap(false) {
			log.Println("Fast reconnect mode — retrying every 1s for up to 30s")
			connected := false
			for i := 0; i < 30; i++ {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Second):
				}
				if err2 := a.connect(ctx); err2 == nil {
					connected = true
					backoff = 5 * time.Second
					break
				}
				if a.conn != nil {
					_ = a.conn.Close()
					a.conn = nil
				}
			}
			if !connected {
				log.Println("Fast reconnect exhausted — falling back to normal backoff")
				backoff = 5 * time.Second
			} else {
				continue
			}
		}

		if err != nil {
			log.Printf("Connection lost: %v — reconnecting in %v", err, backoff)
		} else {
			log.Println("Connection closed, reconnecting...")
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff*2 < maxBackoff {
			backoff *= 2
		} else {
			backoff = maxBackoff
		}
	}
}

func (a *Agent) connect(ctx context.Context) error {
	log.Printf("Connecting to Control Plane: %s", a.cfg.ControlPlane.Address)

	// Setup credentials and keepalive
	var opts []grpc.DialOption

	// Add keepalive parameters to prevent idle connection timeouts
	opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                30 * time.Second, // Send pings every 30 seconds if idle
		Timeout:             10 * time.Second, // Wait 10 seconds for ping ack
		PermitWithoutStream: true,             // Allow pings even without active streams
	}))

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

	// Allow up to 64 MB messages so large command responses (e.g. mysqldump stdout)
	// are not silently dropped by the default 4 MB gRPC limit.
	const maxMsgSize = 64 * 1024 * 1024
	opts = append(opts, grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(maxMsgSize),
		grpc.MaxCallSendMsgSize(maxMsgSize),
	))

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
		AgentVersion: a.version,
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

	// Fire one heartbeat immediately so the agent picks up the
	// op_token_secret (and any pending reconnect signal) within seconds
	// of startup, instead of waiting up to 30s for the first ticker. The
	// secret is in-memory only — every restart with an existing cert
	// arrives with localStore.secret empty, and any destructive DB op
	// during the gap fails with "agent has not received its per-agent
	// op-token secret yet". The bootstrap heartbeat closes that window.
	if reconnect, err := a.tickHeartbeat(ctxWithAuth, client); err != nil {
		log.Printf("Bootstrap heartbeat failed (will retry on tick): %v", err)
	} else if reconnect {
		return nil
	}

	// Heartbeat loop
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.reconnectCh:
			// Command stream sent a RECONNECT command — immediate fast handoff.
			// No need to wait for next heartbeat tick.
			log.Println("Reconnect signal received via command stream — fast handoff")
			a.fastReconnect.Store(true)
			return nil
		case <-heartbeatTicker.C:
			reconnect, err := a.tickHeartbeat(ctxWithAuth, client)
			if err != nil {
				return err
			}
			if reconnect {
				return nil
			}
		}
	}
}

// tickHeartbeat sends one heartbeat and applies any config updates the
// control plane sent back. Returns (reconnect=true, nil) when the panel
// asked for a fast handoff, (false, err) on transport failure, or
// (false, nil) on a successful no-op tick.
func (a *Agent) tickHeartbeat(ctxWithAuth context.Context, client pb.AgentServiceClient) (bool, error) {
	res, err := client.Heartbeat(ctxWithAuth, &pb.HeartbeatRequest{
		AgentId:   a.agentID,
		Timestamp: time.Now().Unix(),
	})
	if err != nil {
		log.Printf("Heartbeat failed: %v", err)
		return false, err
	}

	updates := res.GetConfigUpdates()

	// Per-agent op-token secret for destructive database operations.
	// Refreshed on every heartbeat so a server-side rotation (cert
	// renewal, manual rotate) propagates within one tick. The secret
	// rides the existing mTLS-protected channel; we never persist it
	// to disk.
	if encoded := updates["op_token_secret"]; encoded != "" {
		if raw, decErr := base64.StdEncoding.DecodeString(encoded); decErr == nil {
			database.SetOpTokenSecret(raw)
		} else {
			log.Printf("Heartbeat: op_token_secret base64 decode failed: %v", decErr)
		}
	}

	// Heartbeat-based reconnect signal (fallback when command stream not available)
	if updates["action"] == "reconnect" {
		log.Println("Server requested reconnect via heartbeat — fast handoff")
		a.fastReconnect.Store(true)
		return true, nil
	}

	return false, nil
}

// runCommandStream handles bidirectional command streaming with retry logic
func (a *Agent) runCommandStream(ctx context.Context, client pb.AgentServiceClient, token string) {
	backoff := 5 * time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Println("Starting command stream...")

		// Add auth metadata
		ctxWithAuth := ctx
		if token != "" {
			ctxWithAuth = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}

		stream, err := client.CommandStream(ctxWithAuth)
		if err != nil {
			log.Printf("Failed to open command stream: %v, retrying in %v", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
				continue
			}
		}

		// Reset backoff on successful connection
		backoff = 5 * time.Second

		// Send initial identification message
		err = stream.Send(&pb.CommandResponse{
			CommandId: a.agentID, // Use commandId to pass agentID initially
			Status:    pb.CommandStatus_COMMAND_STATUS_PENDING,
		})
		if err != nil {
			log.Printf("Failed to identify on command stream: %v, retrying...", err)
			continue
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
				errStr := err.Error()
				// If agent not registered, signal to re-register by calling Register again
				if strings.Contains(errStr, "agent not registered") {
					log.Printf("Agent not registered on panel, triggering re-registration...")
					// Re-register with panel
					a.reRegister(ctx, client, token)
				}
				log.Printf("Command stream error: %v, reconnecting in %v...", err, backoff)
				break // Exit inner loop to reconnect
			}

			log.Printf("Received command: id=%s type=%v args=%v", cmd.Id, cmd.Type, cmd.Args)

			// Execute command in goroutine
			go a.executeCommand(ctx, stream, cmd)
		}

		// Wait before reconnecting
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// reRegister re-registers the agent with the panel when connection is lost
func (a *Agent) reRegister(ctx context.Context, client pb.AgentServiceClient, token string) {
	// Add auth metadata
	ctxWithAuth := ctx
	if token != "" {
		ctxWithAuth = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
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

	regReq := &pb.RegisterRequest{
		AgentId:      a.agentID,
		Hostname:     a.hostname,
		Os:           osInfo,
		OsVersion:    osVersion,
		Arch:         arch,
		AgentVersion: a.version,
		Labels:       a.cfg.Agent.Labels,
	}

	regResp, err := client.Register(ctxWithAuth, regReq)
	if err != nil {
		log.Printf("Re-registration failed: %v", err)
		return
	}

	if regResp.Success {
		log.Printf("Re-registered with panel successfully")
	} else {
		log.Printf("Re-registration failed: %s", regResp.Message)
	}
}

// runMetricsStream handles metrics streaming with retry logic
func (a *Agent) runMetricsStream(ctx context.Context, client pb.AgentServiceClient, token string) {
	backoff := 5 * time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Println("Starting metrics stream...")

		// Add auth metadata
		ctxWithAuth := ctx
		if token != "" {
			ctxWithAuth = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}

		stream, err := client.StreamMetrics(ctxWithAuth)
		if err != nil {
			log.Printf("Failed to open metrics stream: %v, retrying in %v", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
				continue
			}
		}

		// Reset backoff on successful connection
		backoff = 5 * time.Second
		log.Println("Metrics stream connected")

		ticker := time.NewTicker(5 * time.Second)
		streamActive := true

		for streamActive {
			select {
			case <-ctx.Done():
				ticker.Stop()
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

				// Get Processes
				var pbProcesses []*pb.ProcessInfo
				for _, p := range metrics.Processes {
					pbProcesses = append(pbProcesses, &pb.ProcessInfo{
						Pid:            p.PID,
						Name:           p.Name,
						Username:       p.Username,
						CpuPercent:     p.CPUPercent,
						MemPercent:     p.MemPercent,
						Rss:            p.RSS,
						CreateTime:     p.CreateTime,
						DiskReadBytes:  p.DiskReadBytes,
						DiskWriteBytes: p.DiskWriteBytes,
						NetConnections: p.NetConnections,
					})
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
					NetErrin:         metrics.NetworkErrin,
					NetErrout:        metrics.NetworkErrout,
					NetDropin:        metrics.NetworkDropin,
					NetDropout:       metrics.NetworkDropout,
					CpuStealTime:     metrics.CPUStealTime,
					CpuIowait:        metrics.CPUIowait,
					Load_1:           metrics.Load1,
					Load_5:           metrics.Load5,
					Load_15:          metrics.Load15,
					UptimeSeconds:    metrics.UptimeSeconds,
					HostInfo:         hostInfo,
					Services:         pbServices,
					Containers:       pbContainers,
					Firewall:         pbFirewall,
					Processes:        pbProcesses,
				}

				if err := stream.Send(pbMetrics); err != nil {
					log.Printf("Failed to send metrics: %v, reconnecting in %v...", err, backoff)
					ticker.Stop()
					streamActive = false
				}
			}
		}

		// Wait before reconnecting
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
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
	case pb.CommandType_COMMAND_TYPE_RECONNECT: // blue-green deploy handoff signal
		// Acknowledge immediately, then trigger fast reconnect via the heartbeat loop.
		_ = stream.Send(&pb.CommandResponse{
			CommandId: cmd.Id,
			Status:    pb.CommandStatus_COMMAND_STATUS_COMPLETED,
			ExitCode:  0,
		})
		// Non-blocking send — reconnectCh has buffer 1
		select {
		case a.reconnectCh <- struct{}{}:
		default:
		}
		return

	case pb.CommandType_COMMAND_TYPE_EXEC:
		// Internal-routed self-management sentinels. The control plane
		// asks the agent to upgrade or uninstall ITSELF via these — and
		// both operations have to do things the executor's hard-block
		// regex explicitly refuses for user-issued commands (curl |
		// bash, systemctl disable cloudnan-agent). Routing through a
		// dedicated agent-internal path means the hard-block stays
		// strict for everything else.
		//
		// SECURITY: gRPC is mTLS-locked, so only an authenticated
		// control plane can deliver these sentinels. A compromised LLM
		// cannot synthesize them — the orchestrator never emits the
		// sentinel string from its tool surface; the only emitter is
		// the BE's case "update" / case "UNINSTALL" command builder.
		if len(cmd.Args) > 0 {
			switch cmd.Args[0] {
			case sentinelSelfUpdate:
				result, execErr = a.runSelfUpdate(execCtx, timeout)
			case sentinelSelfUninstall:
				result, execErr = a.runSelfUninstall(execCtx, timeout)
			default:
				// Pass the command + its args separately so the
				// executor can quote each arg properly. Joining them
				// with spaces destroys quoting and breaks any args
				// containing spaces (e.g. `sh -c "sudo wg show wg0"`
				// would become `sh -c sudo wg show wg0`, where sh
				// receives only "sudo" as the -c command and the
				// rest become shell positional args — making sudo
				// error out with "missing command").
				result, execErr = a.executor.Execute(execCtx, cmd.Args[0], cmd.Args[1:], nil, timeout)
			}
		} else {
			result, execErr = a.executor.Execute(execCtx, "", nil, nil, timeout)
		}

	case pb.CommandType_COMMAND_TYPE_DOCKER:
		// Execute docker command
		// e.g. args=["ps", "-a"] -> "docker ps -a"
		log.Printf("[Agent] Executing docker command: args=%v", cmd.Args)
		result, execErr = a.executor.Execute(execCtx, "docker", cmd.Args, nil, timeout)
		if execErr != nil {
			log.Printf("[Agent] Docker command failed: %v", execErr)
		} else if result != nil {
			log.Printf("[Agent] Docker command result: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
		}

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
		// args[0] = operation
		// args...
		result = a.executeFileCommand(cmd.Args)

	case pb.CommandType_COMMAND_TYPE_SSH:
		// SSH operations
		// args[0] = operation (sync_keys, update_config, get_status, restart)
		if len(cmd.Args) < 1 {
			execErr = fmt.Errorf("SSH command requires operation")
			break
		}
		op := cmd.Args[0]
		result = a.executeSSHCommand(op, cmd.Args[1:])

	case pb.CommandType_COMMAND_TYPE_CHECK_MODULE:
		// Check if modules are installed by checking binary paths on filesystem
		result = a.checkModules(cmd.Args)

	case pb.CommandType_COMMAND_TYPE_DATABASE:
		// Database operations (mysql, postgresql, mariadb).
		// args[0] = operation name (discover, connect, ping, list_dbs,
		// create_db, exec_query, export_dump, ...). For streaming ops the
		// agent emits each chunk as Status=RUNNING and the final result is
		// reported by the common epilogue below.
		if len(cmd.Args) < 1 {
			execErr = fmt.Errorf("DATABASE command requires operation name in args[0]")
			break
		}
		op := cmd.Args[0]
		if database.IsStreamingOp(op) {
			emit := func(chunk string) {
				_ = stream.Send(&pb.CommandResponse{
					CommandId: cmd.Id,
					Status:    pb.CommandStatus_COMMAND_STATUS_RUNNING,
					Stdout:    chunk,
				})
			}
			r := a.dbHandler.ExecuteStreaming(execCtx, cmd.Args, emit)
			result = &executor.Result{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr}
		} else {
			r := a.dbHandler.Execute(execCtx, cmd.Args)
			result = &executor.Result{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr}
		}

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
	if a.cfg.TLS.UseSystemCerts {
		// Cloudflare Tunnel mode: standard TLS with system root CAs.
		// Cloudflare terminates TLS at the edge and presents its own certificate,
		// so we use the system CA pool. mTLS is not used — auth relies on the
		// Bearer token already sent in gRPC metadata.
		log.Println("Using system TLS (Cloudflare Tunnel mode, no mTLS)")
		return &tls.Config{
			InsecureSkipVerify: a.cfg.TLS.InsecureSkipVerify,
		}, nil
	}

	// mTLS mode: custom CA + client certificate
	caCert, err := os.ReadFile(a.cfg.TLS.CACert)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA cert: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA cert")
	}

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

	result.FinishedAt = time.Now()
	return result
}

// runAIFile is a tiny adapter so each ai_* arm reads the same:
// validate the arg count, run the closure, surface either the
// BackupInfo or the error onto the executor.Result. Centralizing
// the boilerplate keeps the switch arms readable.
func runAIFile(result *executor.Result, args []string, minArgs int, op func() (*filesystem.BackupInfo, error)) {
	if len(args) < minArgs {
		result.ExitCode = 1
		result.Stderr = fmt.Sprintf("ai_%s requires at least %d args, got %d", args[0], minArgs, len(args))
		return
	}
	info, err := op()
	if err != nil {
		result.ExitCode = 1
		result.Stderr = err.Error()
		return
	}
	b, mErr := json.Marshal(info)
	if mErr != nil {
		result.ExitCode = 1
		result.Stderr = "marshal backup info: " + mErr.Error()
		return
	}
	result.Stdout = string(b)
}

// executeFileCommand handles File operations
func (a *Agent) executeFileCommand(args []string) *executor.Result {
	result := &executor.Result{
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	}

	if len(args) < 1 {
		result.ExitCode = 1
		result.Stderr = "file command requires operation"
		return result
	}

	op := args[0]

	switch op {
	case "list":
		// args[1] = path
		if len(args) < 2 {
			result.ExitCode = 1
			result.Stderr = "list requires path"
			return result
		}
		log.Printf("[FileCommand] list: requesting path=%q", args[1])
		entries, err := a.fsManager.List(args[1])
		if err != nil {
			log.Printf("[FileCommand] list: error: %v", err)
			result.ExitCode = 1
			result.Stderr = err.Error()
			return result
		}
		log.Printf("[FileCommand] list: got %d entries", len(entries))
		data, _ := json.Marshal(entries)
		result.Stdout = string(data)
		log.Printf("[FileCommand] list: stdout length=%d", len(result.Stdout))

	case "read":
		// args[1] = path
		if len(args) < 2 {
			result.ExitCode = 1
			result.Stderr = "read requires path"
			return result
		}
		content, err := a.fsManager.Read(args[1])
		if err != nil {
			result.ExitCode = 1
			result.Stderr = err.Error()
			return result
		}
		result.Stdout = content

	case "write":
		// args[1] = path, args[2] = content
		if len(args) < 3 {
			result.ExitCode = 1
			result.Stderr = "write requires path and content"
			return result
		}
		if err := a.fsManager.Write(args[1], args[2]); err != nil {
			result.ExitCode = 1
			result.Stderr = err.Error()
			return result
		}
		result.Stdout = "File written successfully"

	case "write_base64":
		// args[1] = path, args[2] = base64_content
		if len(args) < 3 {
			result.ExitCode = 1
			result.Stderr = "write_base64 requires path and content"
			return result
		}
		data, err := base64.StdEncoding.DecodeString(args[2])
		if err != nil {
			result.ExitCode = 1
			result.Stderr = fmt.Sprintf("invalid base64: %v", err)
			return result
		}
		if err := a.fsManager.WriteBytes(args[1], data); err != nil {
			result.ExitCode = 1
			result.Stderr = err.Error()
			return result
		}
		result.Stdout = "File written successfully"

	case "delete":
		// args[1] = path
		if len(args) < 2 {
			result.ExitCode = 1
			result.Stderr = "delete requires path"
			return result
		}
		if err := a.fsManager.Delete(args[1]); err != nil {
			result.ExitCode = 1
			result.Stderr = err.Error()
			return result
		}
		result.Stdout = "File deleted successfully"

	case "mkdir":
		// args[1] = path
		if len(args) < 2 {
			result.ExitCode = 1
			result.Stderr = "mkdir requires path"
			return result
		}
		if err := a.fsManager.Mkdir(args[1]); err != nil {
			result.ExitCode = 1
			result.Stderr = err.Error()
			return result
		}
		result.Stdout = "Directory created successfully"

	case "copy":
		// args[1] = source, args[2] = dest
		if len(args) < 3 {
			result.ExitCode = 1
			result.Stderr = "copy requires source and destination"
			return result
		}
		if err := a.fsManager.Copy(args[1], args[2]); err != nil {
			result.ExitCode = 1
			result.Stderr = err.Error()
			return result
		}
		result.Stdout = "File copied successfully"

	case "ai_backup":
		// Snapshot a target without mutating it. The "prepare" step
		// in the two-step approval flow — the user sees what will
		// be backed up + approves; then a separate "commit" call
		// applies the actual mutation.
		// args[1] = invocation_id, args[2] = abs_path
		runAIFile(result, args, 3, func() (*filesystem.BackupInfo, error) {
			return a.fsManager.AIBackup(args[1], args[2])
		})

	case "ai_write":
		// AI write with backup-first semantics.
		// args[1] = invocation_id, args[2] = abs_path, args[3] = base64_content
		// Returns BackupInfo (JSON) on stdout so the BE can persist it
		// as rollback_recipe + render mode/uid/gid/sha256 on the
		// approval card. Distinct from "write" / "write_base64" so the
		// hardened (blocklisted, atomic, backup-capturing) path can't
		// be confused with the legacy file-browser writer.
		runAIFile(result, args, 4, func() (*filesystem.BackupInfo, error) {
			data, decErr := base64.StdEncoding.DecodeString(args[3])
			if decErr != nil {
				return nil, fmt.Errorf("invalid base64: %v", decErr)
			}
			return a.fsManager.AIWrite(filesystem.AIWriteRequest{
				InvocationID: args[1],
				AbsPath:      args[2],
				Content:      data,
			})
		})

	case "ai_edit":
		// args[1] = invocation_id, args[2] = abs_path,
		// args[3] = old_string (raw), args[4] = new_string (raw),
		// args[5] = "1" for replace_all (optional, default "0").
		runAIFile(result, args, 5, func() (*filesystem.BackupInfo, error) {
			replaceAll := len(args) >= 6 && args[5] == "1"
			return a.fsManager.AIEdit(filesystem.AIEditRequest{
				InvocationID: args[1],
				AbsPath:      args[2],
				OldString:    args[3],
				NewString:    args[4],
				ReplaceAll:   replaceAll,
			})
		})

	case "ai_delete":
		// args[1] = invocation_id, args[2] = abs_path,
		// args[3] = "1" for recursive (required for directories).
		runAIFile(result, args, 3, func() (*filesystem.BackupInfo, error) {
			recursive := len(args) >= 4 && args[3] == "1"
			return a.fsManager.AIDelete(filesystem.AIDeleteRequest{
				InvocationID: args[1],
				AbsPath:      args[2],
				Recursive:    recursive,
			})
		})

	default:
		result.ExitCode = 1
		result.Stderr = fmt.Sprintf("unknown file operation: %s", op)
	}

	result.FinishedAt = time.Now()
	return result
}

// checkModules checks if the requested modules are installed.
// For native modules: verifies binary paths on the filesystem.
// For Docker modules: checks if the container cloudnan-<moduleID> exists.
// Args: list of module IDs (e.g., ["nginx", "qbittorrent", "docker"])
// Returns: JSON in stdout: {"nginx": true, "qbittorrent": false, ...}
func (a *Agent) checkModules(moduleIDs []string) *executor.Result {
	result := &executor.Result{
		StartedAt: time.Now(),
	}

	// Known binary paths for native-installed modules.
	// Multiple paths are checked to cover different installation methods.
	moduleBinaries := map[string][]string{
		"ufw":           {"/usr/sbin/ufw"},
		"fail2ban":      {"/usr/bin/fail2ban-client", "/usr/sbin/fail2ban-client", "/usr/bin/fail2ban-server", "/usr/sbin/fail2ban-server"},
		"wireguard":     {"/usr/bin/wg", "/usr/bin/wg-quick"},
		"openvpn":       {"/usr/sbin/openvpn", "/usr/bin/openvpn", "/sbin/openvpn"},
		"nginx":         {"/usr/sbin/nginx", "/usr/bin/nginx"},
		"apache":        {"/usr/sbin/apache2", "/usr/sbin/httpd"},
		"openlitespeed": {"/usr/local/lsws/bin/openlitespeed", "/usr/local/lsws/bin/lshttpd"},
		"postgresql":    {"/usr/bin/postgres", "/usr/lib/postgresql/*/bin/postgres"},
		"mysql":         {"/usr/sbin/mysqld", "/usr/bin/mysqld"},
		"mariadb":       {"/usr/sbin/mariadbd", "/usr/bin/mariadbd"},
		"redis":         {"/usr/bin/redis-server", "/usr/sbin/redis-server"},
		"docker":        {"/usr/bin/docker"},
		"nodejs":        {"/usr/bin/node", "/usr/local/bin/node"},
		"golang":        {"/usr/local/go/bin/go", "/usr/local/bin/go", "/usr/bin/go", "/snap/bin/go"},
		"rust":          {"/usr/local/bin/rustc", "/root/.cargo/bin/rustc", "/usr/bin/rustc"},
		// Hybrid modules (native or docker)
		"jellyfin":      {"/usr/bin/jellyfin", "/usr/lib/jellyfin/bin/jellyfin"},
		"qbittorrent":   {"/usr/bin/qbittorrent-nox"},
	}

	status := make(map[string]bool)
	for _, moduleID := range moduleIDs {
		// 1. Check native binary paths first (if known)
		found := false
		if paths, ok := moduleBinaries[moduleID]; ok {
			for _, p := range paths {
				if strings.Contains(p, "*") {
					matches, _ := filepath.Glob(p)
					if len(matches) > 0 {
						found = true
						log.Printf("[CheckModule] %s found via glob: %s", moduleID, matches[0])
						break
					}
					continue
				}
				if _, err := os.Stat(p); err == nil {
					found = true
					log.Printf("[CheckModule] %s found at: %s", moduleID, p)
					break
				}
			}
		}

		// 2. If not found natively, check Docker container cloudnan-<moduleID>
		// Try multiple docker binary paths since systemd services may have restricted PATH.
		if !found {
			containerName := "cloudnan-" + moduleID
			for _, dockerBin := range []string{"docker", "/usr/bin/docker", "/usr/local/bin/docker"} {
				out, err := exec.Command(dockerBin, "inspect", "--format={{.State.Running}}", containerName).Output()
				if err == nil && strings.TrimSpace(string(out)) == "true" {
					found = true
					log.Printf("[CheckModule] %s found as Docker container: %s", moduleID, containerName)
					break
				}
			}
		}

		status[moduleID] = found
	}

	jsonBytes, err := json.Marshal(status)
	if err != nil {
		result.ExitCode = 1
		result.Stderr = fmt.Sprintf("failed to marshal status: %v", err)
	} else {
		result.ExitCode = 0
		result.Stdout = string(jsonBytes)
	}

	result.FinishedAt = time.Now()
	return result
}

// ── self-management sentinels ────────────────────────────────────
//
// The control plane delivers self-update / self-uninstall as a
// COMMAND_TYPE_EXEC whose Args[0] is one of these sentinel strings.
// The dispatcher upstream of the executor recognises the sentinel
// and routes to runSelfUpdate / runSelfUninstall — bypassing the
// hard-block regex that (correctly) refuses curl-pipe-bash and
// systemctl-stop-cloudnan-agent for any user-issued command.
const (
	sentinelSelfUpdate    = "__cloudnan_self_update__"
	sentinelSelfUninstall = "__cloudnan_self_uninstall__"
)

// runSelfUpdate downloads the current install script from the
// canonical Cloudnan URL and runs it with --upgrade. This intentionally
// uses the curl-pipe-bash idiom the user-command executor refuses —
// we trust the cloudnan.com origin (mTLS-pinned in the install
// script itself) for our own self-update, and the sentinel routing
// makes it impossible for an LLM-generated tool call to reach this
// code path.
func (a *Agent) runSelfUpdate(ctx context.Context, timeout time.Duration) (*executor.Result, error) {
	const url = "https://cloudnan.com/downloads/install.sh"
	script := fmt.Sprintf(
		"set -e\ncurl -sSL %q | sudo bash -s -- --upgrade",
		url,
	)
	return a.runShellInternal(ctx, script, timeout)
}

// runSelfUninstall stops the agent service, disables it from boot,
// removes the binary + systemd unit + config. Lives behind the
// sentinel for the same reason as runSelfUpdate — the hard-block
// regex correctly refuses any user-issued `systemctl stop cloudnan-
// agent`, but the agent's own legitimate self-removal path needs
// to do exactly that.
func (a *Agent) runSelfUninstall(ctx context.Context, timeout time.Duration) (*executor.Result, error) {
	// Fork the actual cleanup into background so the agent's gRPC
	// stream can return COMPLETED to the control plane BEFORE
	// systemd kills the process out from under us. Without the
	// detached background, the agent dies mid-response and the
	// control plane sees an aborted stream instead of a clean
	// "uninstall scheduled" ack.
	script := "nohup bash -c '" +
		"sleep 3 && " +
		"systemctl stop cloudnan-agent 2>/dev/null; " +
		"systemctl disable cloudnan-agent 2>/dev/null; " +
		"rm -f /etc/systemd/system/cloudnan-agent.service; " +
		"systemctl daemon-reload 2>/dev/null; " +
		"rm -f /usr/local/bin/cloudnan-agent; " +
		"rm -rf /etc/cloudnan" +
		"' >/dev/null 2>&1 & echo 'Uninstall scheduled'"
	return a.runShellInternal(ctx, script, timeout)
}

// runShellInternal runs a shell command DIRECTLY via os/exec,
// skipping the executor's hard-block regex. Reserved for the two
// sentinel paths above — every other code path on the agent must
// go through executor.Execute so the safety guards remain in
// effect.
func (a *Agent) runShellInternal(ctx context.Context, script string, timeout time.Duration) (*executor.Result, error) {
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(cctx, "/bin/sh", "-c", script)
	out, err := c.CombinedOutput()
	result := &executor.Result{
		StartedAt:  time.Now(),
		Stdout:     string(out),
		FinishedAt: time.Now(),
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			result.Error = err
		}
		return result, err
	}
	return result, nil
}
