package monitor

import (
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
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

	// Disk
	Disks []DiskMetrics

	// Network
	NetworkBytesSent uint64
	NetworkBytesRecv uint64

	// Load (Linux only)
	Load1  float64
	Load5  float64
	Load15 float64

	// Uptime
	UptimeSeconds uint64
}

// DiskMetrics contains metrics for a single disk/partition
type DiskMetrics struct {
	MountPoint string
	Device     string
	Total      uint64
	Used       uint64
	Free       uint64
	Percent    float64
}

// Monitor collects system metrics
type Monitor struct {
	lastNetStats *net.IOCountersStat
}

// New creates a new Monitor
func New() *Monitor {
	return &Monitor{}
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
				MountPoint: p.Mountpoint,
				Device:     p.Device,
				Total:      usage.Total,
				Used:       usage.Used,
				Free:       usage.Free,
				Percent:    usage.UsedPercent,
			})
		}
	}

	// Network
	netStats, err := net.IOCounters(false)
	if err == nil && len(netStats) > 0 {
		metrics.NetworkBytesSent = netStats[0].BytesSent
		metrics.NetworkBytesRecv = netStats[0].BytesRecv
	}

	// Load (Linux only)
	loadAvg, err := load.Avg()
	if err == nil {
		metrics.Load1 = loadAvg.Load1
		metrics.Load5 = loadAvg.Load5
		metrics.Load15 = loadAvg.Load15
	}

	// Uptime
	uptime, err := host.Uptime()
	if err == nil {
		metrics.UptimeSeconds = uptime
	}

	return metrics, nil
}
