package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Executor handles command execution on the agent
type Executor struct {
	shell           string
	defaultTimeout  time.Duration
	allowedCommands []string
	blockedCommands []string
}

// Result represents the result of a command execution
type Result struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	StartedAt  time.Time
	FinishedAt time.Time
	Error      error
}

// StreamWriter is called with output chunks during streaming execution
type StreamWriter func(stdout, stderr string)

// New creates a new Executor
func New(shell string, defaultTimeout time.Duration, allowed, blocked []string) *Executor {
	if shell == "" {
		shell = "/bin/bash"
	}
	return &Executor{
		shell:           shell,
		defaultTimeout:  defaultTimeout,
		allowedCommands: allowed,
		blockedCommands: blocked,
	}
}

// Execute runs a command and returns the result
func (e *Executor) Execute(ctx context.Context, command string, args []string, env map[string]string, timeout time.Duration) (*Result, error) {
	// Build full command
	fullCmd := command
	if len(args) > 0 {
		fullCmd = command + " " + strings.Join(args, " ")
	}

	// Check if command is blocked
	if e.isBlocked(fullCmd) {
		return nil, fmt.Errorf("command is blocked: %s", fullCmd)
	}

	// Check if command is allowed (if allowlist is set)
	if len(e.allowedCommands) > 0 && !e.isAllowed(fullCmd) {
		return nil, fmt.Errorf("command is not in allowlist: %s", fullCmd)
	}

	// Set timeout
	if timeout == 0 {
		timeout = e.defaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Create command
	cmd := exec.CommandContext(ctx, e.shell, "-c", fullCmd)

	// Set environment
	if len(env) > 0 {
		for k, v := range env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Capture output
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result := &Result{
		StartedAt: time.Now(),
	}

	// Run command
	err := cmd.Run()
	result.FinishedAt = time.Now()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Error = err
			result.ExitCode = -1
		}
	}

	return result, nil
}

// ExecuteStream runs a command and streams output in real-time
func (e *Executor) ExecuteStream(ctx context.Context, command string, args []string, env map[string]string, timeout time.Duration, writer StreamWriter) (*Result, error) {
	// Build full command
	fullCmd := command
	if len(args) > 0 {
		fullCmd = command + " " + strings.Join(args, " ")
	}

	// Check if command is blocked
	if e.isBlocked(fullCmd) {
		return nil, fmt.Errorf("command is blocked: %s", fullCmd)
	}

	// Set timeout
	if timeout == 0 {
		timeout = e.defaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Create command
	cmd := exec.CommandContext(ctx, e.shell, "-c", fullCmd)

	// Set environment
	if len(env) > 0 {
		for k, v := range env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Create pipes for streaming
	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	result := &Result{
		StartedAt: time.Now(),
	}

	// Start command
	if err := cmd.Start(); err != nil {
		result.Error = err
		return result, err
	}

	// Stream output
	var wg sync.WaitGroup
	var stdoutBuf, stderrBuf bytes.Buffer

	wg.Add(2)
	go e.streamPipe(&wg, stdoutPipe, &stdoutBuf, func(chunk string) {
		if writer != nil {
			writer(chunk, "")
		}
	})
	go e.streamPipe(&wg, stderrPipe, &stderrBuf, func(chunk string) {
		if writer != nil {
			writer("", chunk)
		}
	})

	// Wait for streams to finish
	wg.Wait()

	// Wait for command to complete
	err := cmd.Wait()
	result.FinishedAt = time.Now()
	result.Stdout = stdoutBuf.String()
	result.Stderr = stderrBuf.String()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Error = err
			result.ExitCode = -1
		}
	}

	return result, nil
}

func (e *Executor) streamPipe(wg *sync.WaitGroup, pipe io.ReadCloser, buf *bytes.Buffer, callback func(string)) {
	defer wg.Done()

	chunk := make([]byte, 1024)
	for {
		n, err := pipe.Read(chunk)
		if n > 0 {
			data := string(chunk[:n])
			buf.WriteString(data)
			callback(data)
		}
		if err != nil {
			break
		}
	}
}

func (e *Executor) isBlocked(cmd string) bool {
	cmdLower := strings.ToLower(cmd)
	for _, blocked := range e.blockedCommands {
		if strings.Contains(cmdLower, strings.ToLower(blocked)) {
			return true
		}
	}
	return false
}

func (e *Executor) isAllowed(cmd string) bool {
	cmdLower := strings.ToLower(cmd)
	for _, allowed := range e.allowedCommands {
		if strings.HasPrefix(cmdLower, strings.ToLower(allowed)) {
			return true
		}
	}
	return false
}
