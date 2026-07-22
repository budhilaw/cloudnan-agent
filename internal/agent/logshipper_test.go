package agent

import (
	"strings"
	"testing"

	pb "github.com/cloudnan-tech/cloudnan-agent/proto/agent"
)

func TestBuildLogEntryLevelHeuristics(t *testing.T) {
	cases := []struct {
		name     string
		line     shippedLine
		wantLvl  pb.LogLevel
		wantSrc  string
	}{
		{"stderr maps to warn", shippedLine{source: "docker:web", message: "listening on :3000", isStderr: true}, pb.LogLevel_LOG_LEVEL_WARN, "docker:web"},
		{"error text wins over stdout", shippedLine{source: "docker:web", message: "FATAL: connection refused"}, pb.LogLevel_LOG_LEVEL_ERROR, "docker:web"},
		{"error text wins over stderr", shippedLine{source: "systemd:nginx", message: "panic: nil deref", isStderr: true}, pb.LogLevel_LOG_LEVEL_ERROR, "systemd:nginx"},
		{"plain stdout is info", shippedLine{source: "agent", message: "Registered with Control Plane"}, pb.LogLevel_LOG_LEVEL_INFO, "agent"},
		{"warn text", shippedLine{source: "agent", message: "WARN retrying"}, pb.LogLevel_LOG_LEVEL_WARN, "agent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := buildLogEntry("a-1", c.line)
			if e.Level != c.wantLvl {
				t.Fatalf("level = %v, want %v", e.Level, c.wantLvl)
			}
			if e.Source != c.wantSrc {
				t.Fatalf("source = %q, want %q", e.Source, c.wantSrc)
			}
			if e.AgentId != "a-1" {
				t.Fatalf("agent id = %q", e.AgentId)
			}
		})
	}
}

func TestTruncateLogLineAtRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", maxLogLineBytes) // 2 bytes each → well over the cap
	got := truncateLogLine(s)
	if len(got) > maxLogLineBytes+len(" …[truncated]") {
		t.Fatalf("truncated len %d exceeds cap", len(got))
	}
	if !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("missing truncation marker: %q", got[len(got)-20:])
	}
	// Must remain valid UTF-8 (no split rune).
	if !isValidUTF8(got) {
		t.Fatal("truncation split a UTF-8 rune")
	}
}

func TestLineRingEvictsOldestAndReQueues(t *testing.T) {
	r := newLineRing(2)
	r.push(shippedLine{message: "a"})
	r.push(shippedLine{message: "b"})
	r.push(shippedLine{message: "c"}) // evicts "a"
	got, ok := r.pop()
	if !ok || got.message != "b" {
		t.Fatalf("pop = %q ok=%v, want b", got.message, ok)
	}
	r.pushFront(got) // simulate a failed send re-queue
	got2, _ := r.pop()
	if got2.message != "b" {
		t.Fatalf("re-queued pop = %q, want b", got2.message)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
