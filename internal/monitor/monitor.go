package monitor

import (
	"context"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
)

// SystemInfo contains static system information
type SystemInfo struct {
	Hostname    string
	OS          string
	OSVersion   string
	Arch        string
	NumCPU      int
	TotalMemory uint64
}

// Metrics contains current system metrics
type Metrics struct {
	Timestamp time.Time

	// CPU
	CPUPercent float64
	CPUPerCore []float64

	// Memory
	MemoryTotal     uint64
	MemoryUsed      uint64
	MemoryAvailable uint64
	MemoryPercent   float64
	MemoryCached    uint64
	MemoryBuffers   uint64
	SwapTotal       uint64
	SwapUsed        uint64
	SwapFree        uint64
	SwapPercent     float64

	// Disk
	Disks  []DiskMetrics
	DiskIO []DiskIO

	// Network
	NetworkBytesSent uint64
	NetworkBytesRecv uint64
	NetworkErrin     uint64
	NetworkErrout    uint64
	NetworkDropin    uint64
	NetworkDropout   uint64
	Connections      uint64

	// CPU extended
	CPUStealTime float64
	CPUIowait    float64

	// Load (Linux only)
	Load1  float64
	Load5  float64
	Load15 float64

	// Uptime
	UptimeSeconds uint64
	HostInfo      *HostInfo

	// Processes
	Processes []ProcessInfo
}

// DiskIO contains I/O metrics for a disk
type DiskIO struct {
	Name       string
	ReadCount  uint64
	WriteCount uint64
	ReadBytes  uint64
	WriteBytes uint64
}

// HostInfo contains host details
type HostInfo struct {
	Platform             string
	PlatformFamily       string
	PlatformVersion      string
	KernelVersion        string
	KernelArch           string
	VirtualizationSystem string
	VirtualizationRole   string
	BootTime             uint64
}

// DiskMetrics contains metrics for a single disk/partition
type DiskMetrics struct {
	MountPoint    string
	Device        string
	Total         uint64
	Used          uint64
	Free          uint64
	Percent       float64
	Fstype        string
	InodesTotal   uint64
	InodesUsed    uint64
	InodesPercent float64
}

// Monitor collects system metrics
type Monitor struct {
	mu         sync.Mutex
	prevDiskIO map[int32]ioSnapshot // keyed by PID; tracks cumulative disk I/O for delta

	// Cache for the slow, shell-out-backed collections (systemctl / docker /
	// ufw). These must NOT run on the metrics hot path every interval: an
	// unbounded or slow child process stalls metric delivery (the root cause
	// of the "Agent Not Responding" class of bug — heartbeat keeps working
	// while metrics go stale) and floods the sudo auth log. Each is recomputed
	// at most once per shellCacheTTL, with a hard per-call timeout, and the
	// last good value is served if a refresh times out.
	shellMu         sync.Mutex
	servicesCache   []ServiceInfo
	servicesAt      time.Time
	containersCache []ContainerInfo
	containersAt    time.Time
	firewallCache   *FirewallInfo
	firewallAt      time.Time
}

const (
	// shellCacheTTL is how long a services/containers/firewall snapshot is
	// reused before the next refresh. Slow-changing data — 15s is plenty and
	// keeps the metrics interval (5s) free of synchronous shell-outs.
	shellCacheTTL = 15 * time.Second
	// shellCmdTimeout hard-bounds each systemctl/docker/ufw child so a wedged
	// daemon (slow docker socket, apt-lock contention, D-state) can never block
	// metric collection indefinitely.
	shellCmdTimeout = 5 * time.Second
)

type ioSnapshot struct {
	ReadBytes  uint64
	WriteBytes uint64
}

// New creates a new Monitor
func New() *Monitor {
	return &Monitor{
		prevDiskIO: make(map[int32]ioSnapshot),
	}
}

// GetSystemInfo returns static system information
func (m *Monitor) GetSystemInfo() (*SystemInfo, error) {
	hostInfo, err := host.Info()
	if err != nil {
		return nil, err
	}

	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	return &SystemInfo{
		Hostname:    hostInfo.Hostname,
		OS:          hostInfo.OS,
		OSVersion:   hostInfo.PlatformVersion,
		Arch:        runtime.GOARCH,
		NumCPU:      runtime.NumCPU(),
		TotalMemory: memInfo.Total,
	}, nil
}

// Collect gathers current system metrics
func (m *Monitor) Collect() (*Metrics, error) {
	metrics := &Metrics{
		Timestamp: time.Now(),
	}

	// CPU
	cpuPercent, err := cpu.Percent(time.Second, false)
	if err == nil && len(cpuPercent) > 0 {
		metrics.CPUPercent = cpuPercent[0]
	}

	cpuPerCore, err := cpu.Percent(time.Second, true)
	if err == nil {
		metrics.CPUPerCore = cpuPerCore
	}

	// Memory
	memInfo, err := mem.VirtualMemory()
	if err == nil {
		metrics.MemoryTotal = memInfo.Total
		metrics.MemoryUsed = memInfo.Used
		metrics.MemoryAvailable = memInfo.Available
		metrics.MemoryPercent = memInfo.UsedPercent
		metrics.MemoryCached = memInfo.Cached
		metrics.MemoryBuffers = memInfo.Buffers
	}

	// Swap
	swapInfo, err := mem.SwapMemory()
	if err == nil {
		metrics.SwapTotal = swapInfo.Total
		metrics.SwapUsed = swapInfo.Used
		metrics.SwapFree = swapInfo.Free
		metrics.SwapPercent = swapInfo.UsedPercent
	}

	// Disk
	partitions, err := disk.Partitions(false)
	if err == nil {
		for _, p := range partitions {
			usage, err := disk.Usage(p.Mountpoint)
			if err != nil {
				continue
			}
			metrics.Disks = append(metrics.Disks, DiskMetrics{
				MountPoint:    p.Mountpoint,
				Device:        p.Device,
				Total:         usage.Total,
				Used:          usage.Used,
				Free:          usage.Free,
				Percent:       usage.UsedPercent,
				Fstype:        usage.Fstype,
				InodesTotal:   usage.InodesTotal,
				InodesUsed:    usage.InodesUsed,
				InodesPercent: usage.InodesUsedPercent,
			})
		}
	}

	// Disk I/O
	diskIO, err := disk.IOCounters()
	if err == nil {
		for name, io := range diskIO {
			metrics.DiskIO = append(metrics.DiskIO, DiskIO{
				Name:       name,
				ReadCount:  io.ReadCount,
				WriteCount: io.WriteCount,
				ReadBytes:  io.ReadBytes,
				WriteBytes: io.WriteBytes,
			})
		}
	}

	// Network
	netStats, err := net.IOCounters(false)
	if err == nil && len(netStats) > 0 {
		metrics.NetworkBytesSent = netStats[0].BytesSent
		metrics.NetworkBytesRecv = netStats[0].BytesRecv
		metrics.NetworkErrin = netStats[0].Errin
		metrics.NetworkErrout = netStats[0].Errout
		metrics.NetworkDropin = netStats[0].Dropin
		metrics.NetworkDropout = netStats[0].Dropout
	}

	// CPU steal time and iowait (Linux only)
	cpuTimes, err := cpu.Times(false)
	if err == nil && len(cpuTimes) > 0 {
		total := cpuTimes[0].User + cpuTimes[0].System + cpuTimes[0].Idle + cpuTimes[0].Nice +
			cpuTimes[0].Iowait + cpuTimes[0].Irq + cpuTimes[0].Softirq + cpuTimes[0].Steal
		if total > 0 {
			metrics.CPUStealTime = (cpuTimes[0].Steal / total) * 100
			metrics.CPUIowait = (cpuTimes[0].Iowait / total) * 100
		}
	}

	// Network Connections — count only.
	//
	// net.Connections("all") resolves every socket's owning UID/PID by walking
	// /proc/*/fd, which on a busy box (e.g. a Postgres host with hundreds of
	// connections) takes seconds to minutes and was a primary cause of stalled
	// metric delivery. We only need the count, so use the UID-less variant,
	// which just reads /proc/net/{tcp,tcp6,udp,...} and skips the expensive
	// inode→PID mapping.
	conns, err := net.ConnectionsWithoutUids("all")
	if err == nil {
		metrics.Connections = uint64(len(conns))
	}

	// Load (Linux only)
	loadAvg, err := load.Avg()
	if err == nil {
		metrics.Load1 = loadAvg.Load1
		metrics.Load5 = loadAvg.Load5
		metrics.Load15 = loadAvg.Load15
	}

	// Host Info
	hostInfo, err := host.Info()
	if err == nil {
		metrics.UptimeSeconds = hostInfo.Uptime
		metrics.HostInfo = &HostInfo{
			Platform:             hostInfo.Platform,
			PlatformFamily:       hostInfo.PlatformFamily,
			PlatformVersion:      hostInfo.PlatformVersion,
			KernelVersion:        hostInfo.KernelVersion,
			KernelArch:           hostInfo.KernelArch,
			VirtualizationSystem: hostInfo.VirtualizationSystem,
			VirtualizationRole:   hostInfo.VirtualizationRole,
			BootTime:             hostInfo.BootTime,
		}
	}

	// Top Processes
	metrics.Processes = m.GetTopProcesses(30)

	return metrics, nil
}

// GetTopProcesses returns up to 30 processes: top 20 by CPU + top 10 by disk I/O (deduplicated).
// Disk I/O is expressed as bytes delta since the last call to this function.
// Network connection count is collected for the final merged set only (expensive per-process call).
// A 8-second timeout guards against D-state processes that would block /proc reads indefinitely.
func (m *Monitor) GetTopProcesses(limit int) []ProcessInfo {
	type procResult struct {
		procs []ProcessInfo
		snaps map[int32]ioSnapshot
	}
	ch := make(chan procResult, 1)

	// Snapshot previous disk I/O before spawning goroutine
	m.mu.Lock()
	prevSnaps := m.prevDiskIO
	m.mu.Unlock()

	go func() {
		procs, err := process.Processes()
		if err != nil {
			ch <- procResult{}
			return
		}
		newSnaps := make(map[int32]ioSnapshot, len(procs))
		var all []ProcessInfo
		for _, p := range procs {
			name, _ := p.Name()
			cpuPct, _ := p.CPUPercent()
			memPct, _ := p.MemoryPercent()
			memInfo, _ := p.MemoryInfo()
			createTime, _ := p.CreateTime()
			username, _ := p.Username()

			var rss uint64
			if memInfo != nil {
				rss = memInfo.RSS
			}

			// Disk I/O delta — reads /proc/PID/io, can block on D-state processes.
			// We rely on the outer timeout to bound total execution; individual calls
			// that block will be abandoned when the goroutine result is discarded.
			var diskReadDelta, diskWriteDelta uint64
			if ioc, ioErr := p.IOCounters(); ioErr == nil && ioc != nil {
				cur := ioSnapshot{ReadBytes: ioc.ReadBytes, WriteBytes: ioc.WriteBytes}
				newSnaps[p.Pid] = cur
				if prev, ok := prevSnaps[p.Pid]; ok {
					if cur.ReadBytes >= prev.ReadBytes {
						diskReadDelta = cur.ReadBytes - prev.ReadBytes
					}
					if cur.WriteBytes >= prev.WriteBytes {
						diskWriteDelta = cur.WriteBytes - prev.WriteBytes
					}
				}
			}

			all = append(all, ProcessInfo{
				PID:            p.Pid,
				Name:           name,
				Username:       username,
				CPUPercent:     cpuPct,
				MemPercent:     float64(memPct),
				RSS:            rss,
				CreateTime:     createTime,
				DiskReadBytes:  diskReadDelta,
				DiskWriteBytes: diskWriteDelta,
			})
		}
		ch <- procResult{procs: all, snaps: newSnaps}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	var all []ProcessInfo
	select {
	case r := <-ch:
		all = r.procs
		m.mu.Lock()
		m.prevDiskIO = r.snaps
		m.mu.Unlock()
	case <-ctx.Done():
		// D-state process blocked collection; return empty and let goroutine finish on its own
		return nil
	}

	// Top 20 by CPU
	cpuSorted := make([]ProcessInfo, len(all))
	copy(cpuSorted, all)
	sort.Slice(cpuSorted, func(i, j int) bool {
		return cpuSorted[i].CPUPercent > cpuSorted[j].CPUPercent
	})
	topCPU := 20
	if len(cpuSorted) < topCPU {
		topCPU = len(cpuSorted)
	}

	// Top 10 by disk I/O (read + write delta)
	diskSorted := make([]ProcessInfo, len(all))
	copy(diskSorted, all)
	sort.Slice(diskSorted, func(i, j int) bool {
		ti := diskSorted[i].DiskReadBytes + diskSorted[i].DiskWriteBytes
		tj := diskSorted[j].DiskReadBytes + diskSorted[j].DiskWriteBytes
		return ti > tj
	})
	topDisk := 10
	if len(diskSorted) < topDisk {
		topDisk = len(diskSorted)
	}

	// Merge (deduplicate by PID)
	seen := make(map[int32]bool, topCPU+topDisk)
	result := make([]ProcessInfo, 0, topCPU+topDisk)
	for _, p := range cpuSorted[:topCPU] {
		if !seen[p.PID] {
			seen[p.PID] = true
			result = append(result, p)
		}
	}
	for _, p := range diskSorted[:topDisk] {
		if !seen[p.PID] {
			seen[p.PID] = true
			result = append(result, p)
		}
	}

	// Collect net connections for the final set only (expensive — /proc/<pid>/net/*)
	for i := range result {
		if proc, procErr := process.NewProcess(result[i].PID); procErr == nil {
			if conns, connErr := proc.Connections(); connErr == nil {
				result[i].NetConnections = uint32(len(conns))
			}
		}
	}

	// Apply caller's limit cap
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}

	return result
}

// ServiceInfo contains service status
type ServiceInfo struct {
	Name        string
	Status      string
	Restarts    int32
	LastRestart string
}

// ContainerInfo contains container status
type ContainerInfo struct {
	ID        string
	Name      string
	Image     string
	Status    string
	Health    string
	Restarts  int32
	StartedAt string
}

// ProcessInfo contains process details
type ProcessInfo struct {
	PID            int32
	Name           string
	Username       string
	CPUPercent     float64
	MemPercent     float64
	RSS            uint64
	CreateTime     int64
	DiskReadBytes  uint64 // bytes read since last poll (delta)
	DiskWriteBytes uint64 // bytes written since last poll (delta)
	NetConnections uint32 // active TCP/UDP connections
}

// GetServices returns status of systemd services. Cached for shellCacheTTL and
// hard-bounded by shellCmdTimeout so it never blocks the metrics hot path; on a
// transient timeout the last good snapshot is served instead of an empty list.
func (m *Monitor) GetServices() []ServiceInfo {
	m.shellMu.Lock()
	if m.servicesCache != nil && time.Since(m.servicesAt) < shellCacheTTL {
		cached := m.servicesCache
		m.shellMu.Unlock()
		return cached
	}
	m.shellMu.Unlock()

	fresh, ok := m.collectServices()

	m.shellMu.Lock()
	defer m.shellMu.Unlock()
	if ok {
		m.servicesCache = fresh
		m.servicesAt = time.Now()
		return fresh
	}
	if m.servicesCache != nil {
		return m.servicesCache
	}
	return []ServiceInfo{}
}

// collectServices does the actual systemctl shell-out. The bool is false only
// on a command error/timeout (so the caller keeps the previous cache); a host
// with no systemd legitimately returns (empty, true).
func (m *Monitor) collectServices() ([]ServiceInfo, bool) {
	services := []ServiceInfo{}

	// Check if systemd is available
	_, err := exec.LookPath("systemctl")
	if err != nil {
		return services, true
	}

	// List active and failed services
	// systemctl list-units --type=service --state=active,failed --no-pager --no-legend --plain
	ctx, cancel := context.WithTimeout(context.Background(), shellCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "list-units", "--type=service", "--state=active,failed", "--no-pager", "--no-legend", "--plain")
	output, err := cmd.Output()
	if err != nil {
		return nil, false
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		name := fields[0]
		// Remove .service suffix for cleaner display
		displayName := strings.TrimSuffix(name, ".service")

		activeState := fields[2] // active, failed
		subState := fields[3]    // running, exited, failed, dead

		var status string
		switch {
		case activeState == "active" && subState == "running":
			status = "running"
		case activeState == "failed":
			status = "failed"
		case activeState == "active" && subState == "exited":
			status = "exited"
		default:
			status = activeState
		}

		// Filter out some very transient or system-internal services if needed,
		// but for now we list what systemctl returns as "active/failed".
		// Maybe filter out "user@" services which are per-user
		if strings.HasPrefix(name, "user@") || strings.HasPrefix(name, "session-") {
			continue
		}

		services = append(services, ServiceInfo{
			Name:   displayName,
			Status: status,
		})
	}

	return services, true
}

// GetContainers returns status of Docker containers. Cached for shellCacheTTL
// and hard-bounded by shellCmdTimeout: a wedged docker daemon (slow/hung
// socket) can no longer stall metric collection.
func (m *Monitor) GetContainers() []ContainerInfo {
	m.shellMu.Lock()
	if m.containersCache != nil && time.Since(m.containersAt) < shellCacheTTL {
		cached := m.containersCache
		m.shellMu.Unlock()
		return cached
	}
	m.shellMu.Unlock()

	fresh, ok := m.collectContainers()

	m.shellMu.Lock()
	defer m.shellMu.Unlock()
	if ok {
		m.containersCache = fresh
		m.containersAt = time.Now()
		return fresh
	}
	if m.containersCache != nil {
		return m.containersCache
	}
	return []ContainerInfo{}
}

// collectContainers does the actual docker shell-out. The bool is false only on
// a command error/timeout. A host with no docker installed also returns
// (empty, false) — but since there's no cache to fall back to, GetContainers
// still yields an empty list, which is correct.
func (m *Monitor) collectContainers() ([]ContainerInfo, bool) {
	containers := []ContainerInfo{}

	// Skip entirely if docker isn't installed — avoids spawning a doomed child
	// (and a misleading error) every refresh on the many hosts without docker.
	if _, err := exec.LookPath("docker"); err != nil {
		return containers, true
	}

	// Use docker ps to get container info
	// format: ID|Names|Image|Status|State
	ctx, cancel := context.WithTimeout(context.Background(), shellCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.Status}}|{{.State}}")
	output, err := cmd.Output()
	if err != nil {
		// Docker daemon unreachable / timed out
		return nil, false
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 5 {
			continue
		}

		id := parts[0]
		name := parts[1]
		image := parts[2]
		state := parts[4]

		var status string
		switch state {
		case "running":
			status = "running"
		case "paused":
			status = "paused"
		case "exited":
			status = "exited"
		default:
			status = "stopped"
		}

		containers = append(containers, ContainerInfo{
			ID:     id,
			Name:   name,
			Image:  image,
			Status: status,
		})
	}
	return containers, true
}
