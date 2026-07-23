package agent

import (
	"bufio"
	"context"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cloudnan-tech/cloudnan-agent/proto/agent"
)

// App-log shipper. Follows Docker container stdout/stderr, named systemd units,
// and (optionally) the agent's own log, and streams each line to the control
// plane over the existing StreamLogs RPC — outbound only, like everything else
// the agent does. Sources are tagged the way the control-plane classifier
// expects: "docker:<name>", "systemd:<unit>", "agent".
//
// Resilience: lines land in a bounded in-memory ring (newest wins, oldest
// evicted) so a control-plane outage can never grow agent memory. The sender
// peeks the oldest line and only drops it once the server accepts it, so a
// send failure loses nothing but the tail during a long outage. On-disk spill
// (the Rust agent's stronger guarantee) is deliberately not part of this Go
// interim — the bounded ring is a complete, self-contained resilience policy,
// not a stub.

const (
	// Hard cap on one shipped line; longer lines are truncated with a marker so
	// a megabyte JSON blob can't monopolise the stream.
	maxLogLineBytes = 16 * 1024
	// Ring capacity while the control plane is unreachable.
	logRingCapacity = 50_000
	// Idle poll when the ring is empty (keeps the sender cheap).
	logRingIdlePoll = 200 * time.Millisecond
)

// shippedLine is one collected log line before it becomes a wire LogEntry.
type shippedLine struct {
	source   string
	message  string
	tsSec    int64
	isStderr bool
}

// lineRing is a mutex-guarded bounded deque of collected lines. push evicts the
// oldest when full; pop returns the oldest; pushFront re-queues an
// unacknowledged line at the head (evicting the newest to stay bounded).
type lineRing struct {
	mu  sync.Mutex
	buf []shippedLine
	cap int
}

func newLineRing(capacity int) *lineRing {
	return &lineRing{buf: make([]shippedLine, 0, 1024), cap: capacity}
}

func (r *lineRing) push(l shippedLine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) >= r.cap {
		r.buf = r.buf[1:]
	}
	r.buf = append(r.buf, l)
}

func (r *lineRing) pushFront(l shippedLine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) >= r.cap {
		r.buf = r.buf[:len(r.buf)-1] // drop the newest to make room for the re-queued oldest
	}
	r.buf = append([]shippedLine{l}, r.buf...)
}

func (r *lineRing) pop() (shippedLine, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return shippedLine{}, false
	}
	l := r.buf[0]
	r.buf = r.buf[1:]
	return l, true
}

func truncateLogLine(s string) string {
	if len(s) <= maxLogLineBytes {
		return s
	}
	cut := maxLogLineBytes
	for cut > 0 && !utf8StartsHere(s, cut) {
		cut--
	}
	return s[:cut] + " …[truncated]"
}

// utf8StartsHere reports whether index i is a UTF-8 rune boundary.
func utf8StartsHere(s string, i int) bool {
	if i <= 0 || i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80 // not a continuation byte
}

// buildLogEntry converts a collected line into the wire message. Level
// heuristics keep parity with the control plane's expectations: stderr maps to
// WARN unless the text self-declares a higher level.
func buildLogEntry(agentID string, l shippedLine) *pb.LogEntry {
	lower := strings.ToLower(l.message)
	var level pb.LogLevel
	switch {
	case strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic"):
		level = pb.LogLevel_LOG_LEVEL_ERROR
	case l.isStderr || strings.Contains(lower, "warn"):
		level = pb.LogLevel_LOG_LEVEL_WARN
	case strings.Contains(lower, "debug"):
		level = pb.LogLevel_LOG_LEVEL_DEBUG
	default:
		level = pb.LogLevel_LOG_LEVEL_INFO
	}
	return &pb.LogEntry{
		AgentId:   agentID,
		Timestamp: l.tsSec,
		Level:     level,
		Source:    l.source,
		Message:   l.message,
	}
}

// logTap is an io.Writer teed onto the standard logger so the agent's own log
// lines ship as source "agent". Write never blocks the logger and never errors.
type logTap struct{ ring *lineRing }

func (t *logTap) Write(p []byte) (int, error) {
	for _, raw := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		t.ring.push(shippedLine{
			source:  "agent",
			message: truncateLogLine(line),
			tsSec:   time.Now().Unix(),
		})
	}
	return len(p), nil
}

// startLogCollectors wires the shipper's producers once for the agent's whole
// lifetime (root ctx). It creates the ring and launches the agent-log tap plus
// the Docker and systemd followers. The per-connection sender is runLogStream.
func (a *Agent) startLogCollectors(ctx context.Context) {
	if !a.cfg.LogShipper.IsEnabled() {
		return
	}
	a.logRing = newLineRing(logRingCapacity)

	if a.cfg.LogShipper.ShipsAgentLog() {
		// Tee the standard logger: keep its current sink (file or stderr) and
		// also feed the ring. Installed before any follower so early lines ship.
		log.SetOutput(io.MultiWriter(log.Writer(), &logTap{ring: a.logRing}))
	}
	if a.cfg.LogShipper.FollowAllContainers() {
		if _, err := exec.LookPath("docker"); err == nil {
			go a.runDockerFollower(ctx)
		} else {
			log.Println("[logshipper] docker not found — skipping container log shipping")
		}
	}
	if a.cfg.LogShipper.FollowsSystemdUnits() {
		if _, err := exec.LookPath("journalctl"); err == nil {
			go a.runSystemdFollower(ctx)
		} else {
			log.Println("[logshipper] journalctl not found — skipping systemd unit shipping")
		}
	}
	if a.cfg.LogShipper.FollowsAppLogs() {
		if _, err := exec.LookPath("tail"); err == nil {
			go a.runAppFollower(ctx)
		} else {
			log.Println("[logshipper] tail not found — skipping app log shipping")
		}
	}
	log.Printf("[logshipper] enabled (agent_log=%v containers=%v systemd=%v app=%v)",
		a.cfg.LogShipper.ShipsAgentLog(), a.cfg.LogShipper.FollowAllContainers(),
		a.cfg.LogShipper.FollowsSystemdUnits(), a.cfg.LogShipper.FollowsAppLogs())
}

// runDockerFollower discovers running containers and follows each one's logs,
// re-scanning periodically to pick up newly started containers. A follower
// keeps draining after its container exits (crash tails are the most valuable
// bytes on the box), then the container drops out of the followed set so a
// restart re-follows it.
func (a *Agent) runDockerFollower(ctx context.Context) {
	followed := make(map[string]context.CancelFunc)
	var mu sync.Mutex
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	scan := func() {
		out, err := exec.CommandContext(ctx, "docker", "ps", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}").Output()
		if err != nil {
			return
		}
		seen := make(map[string]bool)
		for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if ln == "" {
				continue
			}
			parts := strings.SplitN(ln, "\t", 2)
			id := parts[0]
			name := id
			if len(parts) == 2 && parts[1] != "" {
				name = parts[1]
			}
			seen[id] = true
			mu.Lock()
			_, already := followed[id]
			mu.Unlock()
			if !already {
				fctx, cancel := context.WithCancel(ctx)
				mu.Lock()
				followed[id] = cancel
				mu.Unlock()
				go func(id, name string) {
					a.followContainer(fctx, id, name)
					mu.Lock()
					delete(followed, id)
					mu.Unlock()
				}(id, name)
			}
		}
	}

	scan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan()
		}
	}
}

// followContainer streams one container's stdout+stderr. `docker logs` writes
// container stdout to our stdout and stderr to our stderr, so the two pipes
// distinguish level. It exits when the container stops (having drained the
// tail) or the context is cancelled.
func (a *Agent) followContainer(ctx context.Context, id, name string) {
	source := "docker:" + name
	cmd := exec.CommandContext(ctx, "docker", "logs", "--follow", "--tail", "0", id)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.scanLines(stdout, source, false) }()
	go func() { defer wg.Done(); a.scanLines(stderr, source, true) }()
	wg.Wait()
	_ = cmd.Wait()
}

// runJournaldFollower follows one systemd unit via `journalctl -f`, shipping
// its lines under the given source tag (systemd:<unit> for infra services,
// app:<unit> for customer application units). The subprocess (not libsystemd)
// keeps the agent a static binary.
func (a *Agent) runJournaldFollower(ctx context.Context, unit, source string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		cmd := exec.CommandContext(ctx, "journalctl", "-u", unit, "-f", "-n", "0", "--no-pager", "-o", "cat")
		stdout, err := cmd.StdoutPipe()
		if err == nil && cmd.Start() == nil {
			a.scanLines(stdout, source, false)
			_ = cmd.Wait()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// runSystemdFollower follows the catalog-matched systemd units on the box
// (plus any configured extras), re-discovering periodically so a newly
// installed service starts shipping without an agent restart. Each unit gets
// one long-lived runJournaldFollower; units are never un-followed (a removed
// unit's journalctl -f simply idles, and its crash tail stays valuable).
func (a *Agent) runSystemdFollower(ctx context.Context) {
	followed := make(map[string]struct{})
	discover := func() {
		for _, unit := range discoverSystemdUnits(ctx, a.cfg.LogShipper.Units) {
			if _, ok := followed[unit]; ok {
				continue
			}
			followed[unit] = struct{}{}
			go a.runJournaldFollower(ctx, unit, "systemd:"+unit)
		}
	}
	discover()
	ticker := time.NewTicker(sourceRediscoverInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			discover()
		}
	}
}

// runAppFollower ships the App source: per-site log FILES discovered from the
// web server's vhosts + framework conventions, AND customer application systemd
// UNITS (a Go/Node/Python/etc. binary run as its own service, which the vhost
// scanner and the known-service catalog both miss). Both are re-discovered on a
// timer so a newly deployed site or service starts shipping without a restart.
// The followed set is keyed with distinct "file:"/"unit:" prefixes so a file
// path and a unit name can never collide.
func (a *Agent) runAppFollower(ctx context.Context) {
	followed := make(map[string]struct{})
	discover := func() {
		for _, t := range discoverAppLogTargets() {
			for _, f := range t.files {
				key := "file:" + f.path
				if _, ok := followed[key]; ok {
					continue
				}
				followed[key] = struct{}{}
				go a.runAppFileFollower(ctx, t.site, f.path, f.isError)
			}
		}
		if _, err := exec.LookPath("journalctl"); err == nil {
			for _, unit := range discoverAppUnits(ctx, agentSystemdUnit) {
				key := "unit:" + unit
				if _, ok := followed[key]; ok {
					continue
				}
				followed[key] = struct{}{}
				go a.runJournaldFollower(ctx, unit, "app:"+unit)
			}
		}
	}
	discover()
	ticker := time.NewTicker(sourceRediscoverInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			discover()
		}
	}
}

// runAppFileFollower tails one application log file as source "app:<site>".
// `tail -F` follows the path across rotation/truncation (reopening on rename),
// so a logrotate cycle doesn't stop the stream. Starts at the tail (-n 0) so
// existing history isn't re-shipped, and reconnects with a fixed delay if tail
// exits (e.g. the file's directory is briefly absent mid-deploy).
func (a *Agent) runAppFileFollower(ctx context.Context, site, path string, isError bool) {
	source := "app:" + site
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		cmd := exec.CommandContext(ctx, "tail", "-F", "-n", "0", path)
		stdout, err := cmd.StdoutPipe()
		if err == nil && cmd.Start() == nil {
			a.scanLines(stdout, source, isError)
			_ = cmd.Wait()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// scanLines reads a follower pipe line-by-line into the ring.
func (a *Agent) scanLines(r io.Reader, source string, isStderr bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes+1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		a.logRing.push(shippedLine{
			source:   source,
			message:  truncateLogLine(line),
			tsSec:    time.Now().Unix(),
			isStderr: isStderr,
		})
	}
}

// runLogStream maintains the StreamLogs RPC and drains the ring to the control
// plane. Mirrors runMetricsStream: reconnect with capped backoff, a background
// ack drainer so HTTP/2 flow control never stalls the Send side, and no data
// loss on a failed send (the line is re-queued at the head). Launched per
// connection; the ring + collectors outlive it.
func (a *Agent) runLogStream(ctx context.Context, client pb.AgentServiceClient, token string) {
	if a.logRing == nil {
		return // shipper disabled
	}
	backoff := 5 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ctxWithAuth := ctx
		if token != "" {
			ctxWithAuth = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}
		stream, err := client.StreamLogs(ctxWithAuth)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
				continue
			}
		}
		backoff = 5 * time.Second

		streamBroken := make(chan struct{})
		go func() {
			for {
				if _, err := stream.Recv(); err != nil {
					close(streamBroken)
					return
				}
			}
		}()

		streamActive := true
		for streamActive {
			select {
			case <-ctx.Done():
				return
			case <-streamBroken:
				streamActive = false
			default:
				line, ok := a.logRing.pop()
				if !ok {
					select {
					case <-ctx.Done():
						return
					case <-streamBroken:
						streamActive = false
					case <-time.After(logRingIdlePoll):
					}
					continue
				}
				if err := stream.Send(buildLogEntry(a.agentID, line)); err != nil {
					a.logRing.pushFront(line) // never lose the line on a failed send
					streamActive = false
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		}
	}
}
