// Package database — handler.go.
//
// COMMAND_TYPE_DATABASE dispatcher. The agent's command loop calls into
// Execute / ExecuteStreaming with args[0] = operation name and (for ops
// that take a payload) args[1] = JSON-encoded protobuf request. Responses
// are written back as JSON-encoded protobuf in Result.Stdout.
//
// PR 2 implements: discover, connect, disconnect, ping. PR 3 adds
// list_dbs, list_users, create_db, drop_db, create_user, grant, drop_user.
// PR 4 wires exec_query (streaming NDJSON of pb.DatabaseQueryResultChunk).
// PR 5 wires export_dump (streaming NDJSON of pb.DatabaseExportProgress
// with the dump piped through AES-256-GCM into S3-compatible storage).
//
// Destructive operations (drop_db, drop_user) require a confirmation
// token (HMAC-SHA256 of a payload binding op + instance + target +
// expiry) minted by the control plane and verified locally before any
// SQL is executed — see token.go.
//
// The handler holds a lazily-opened *Vault. We open the vault on first DB
// op rather than at agent startup so a host that never uses DB management
// never pays the cost of creating /var/lib/cloudnan-agent and a key file.
package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/cloudnan-tech/cloudnan-agent/proto/agent"
)

// Handler dispatches COMMAND_TYPE_DATABASE operations.
type Handler struct {
	once  sync.Once
	vault *Vault
	err   error
}

// NewHandler constructs a Handler. The credential vault is opened lazily
// on the first DB operation that needs it.
func NewHandler() *Handler {
	return &Handler{}
}

// Result is what the agent dispatcher returns up to executeCommand.
// Stdout/Stderr are placed verbatim into the final CommandResponse fields.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Execute is the unary entry point. For ops that stream (exec_query,
// export_dump) callers should use ExecuteStreaming instead — calling
// Execute on a streaming op returns an error result.
func (h *Handler) Execute(ctx context.Context, args []string) *Result {
	if len(args) < 1 {
		return errResult("database command requires operation name in args[0]")
	}
	op := args[0]
	switch op {
	case "discover":
		return h.opDiscover(ctx, args)
	case "connect":
		return h.opConnect(ctx, args)
	case "disconnect":
		return h.opDisconnect(ctx, args)
	case "ping":
		return h.opPing(ctx, args)
	case "list_dbs":
		return h.opListDBsCmd(ctx, args)
	case "list_users":
		return h.opListUsersCmd(ctx, args)
	case "create_db":
		return h.opCreateDBCmd(ctx, args)
	case "drop_db":
		return h.opDropDBCmd(ctx, args)
	case "create_user":
		return h.opCreateUserCmd(ctx, args)
	case "grant":
		return h.opGrantCmd(ctx, args)
	case "drop_user":
		return h.opDropUserCmd(ctx, args)
	case "provision_dump_user":
		return h.opProvisionDumpUserCmd(ctx, args)
	case "exec_query", "export_dump", "restore_dump":
		return errResult(fmt.Sprintf("database op %q is streaming; caller must use ExecuteStreaming", op))
	default:
		return errResult(fmt.Sprintf("unknown database op %q", op))
	}
}

// ExecuteStreaming handles streaming ops. emit is called once per chunk and
// is also forwarded to the transport for live progressive delivery on the
// single-host path; we additionally accumulate every chunk into the Result's
// Stdout so the cross-host dispatch path (pg_notify only forwards terminal
// responses, not intermediate chunks) still receives the full payload in the
// final CommandResponse.
//
// On the control plane side, parseChunkLines splits the terminal Stdout on
// '\n' and feeds each line to onChunk, so the rendering is identical
// regardless of whether the chunks arrived progressively or in one shot.
func (h *Handler) ExecuteStreaming(
	ctx context.Context,
	args []string,
	emit func(stdoutChunk string),
) *Result {
	if len(args) < 1 {
		return errResult("database command requires operation name in args[0]")
	}
	op := args[0]

	// Wrap emit so every chunk is also captured into a buffer that we attach
	// to Result.Stdout. The transport sees the same chunks live; the buffered
	// copy travels in the final terminal CommandResponse.
	var buf strings.Builder
	bufferedEmit := func(chunk string) {
		buf.WriteString(chunk)
		if emit != nil {
			emit(chunk)
		}
	}

	switch op {
	case "exec_query":
		if len(args) < 2 {
			return errResult("exec_query: missing JSON request in args[1]")
		}
		req := &pb.DatabaseExecQueryRequest{}
		if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
			return errResult(fmt.Sprintf("exec_query: unmarshal request: %v", err))
		}
		if err := h.opExecQuery(ctx, req, bufferedEmit); err != nil {
			return errResult(fmt.Sprintf("exec_query: %v", err))
		}
		return &Result{ExitCode: 0, Stdout: buf.String()}
	case "export_dump":
		if err := h.opExportDump(ctx, args, bufferedEmit); err != nil {
			return errResult(fmt.Sprintf("export_dump: %v", err))
		}
		return &Result{ExitCode: 0, Stdout: buf.String()}
	case "restore_dump":
		if err := h.opRestoreDump(ctx, args, bufferedEmit); err != nil {
			return errResult(fmt.Sprintf("restore_dump: %v", err))
		}
		return &Result{ExitCode: 0, Stdout: buf.String()}
	case "supabase_migrate_dump":
		if err := h.opSupabaseDump(ctx, args, bufferedEmit); err != nil {
			return errResult(fmt.Sprintf("supabase_migrate_dump: %v", err))
		}
		return &Result{ExitCode: 0, Stdout: buf.String()}
	case "supabase_migrate_restore":
		if err := h.opSupabaseRestore(ctx, args, bufferedEmit); err != nil {
			return errResult(fmt.Sprintf("supabase_migrate_restore: %v", err))
		}
		return &Result{ExitCode: 0, Stdout: buf.String()}
	case "supabase_storage_sync":
		if err := h.opSupabaseStorageSync(ctx, args, bufferedEmit); err != nil {
			return errResult(fmt.Sprintf("supabase_storage_sync: %v", err))
		}
		return &Result{ExitCode: 0, Stdout: buf.String()}
	case "supabase_verify":
		if err := h.opSupabaseVerify(ctx, args, bufferedEmit); err != nil {
			return errResult(fmt.Sprintf("supabase_verify: %v", err))
		}
		return &Result{ExitCode: 0, Stdout: buf.String()}
	case "supabase_deploy_functions":
		if err := h.opSupabaseDeployFunctions(ctx, args, bufferedEmit); err != nil {
			return errResult(fmt.Sprintf("supabase_deploy_functions: %v", err))
		}
		return &Result{ExitCode: 0, Stdout: buf.String()}
	default:
		return errResult(fmt.Sprintf("op %q is not streaming or is unknown", op))
	}
}

// IsStreamingOp reports whether the named op uses streaming chunks.
func IsStreamingOp(op string) bool {
	switch op {
	case "exec_query", "export_dump", "restore_dump",
		"supabase_migrate_dump", "supabase_migrate_restore",
		"supabase_storage_sync", "supabase_verify", "supabase_deploy_functions":
		return true
	}
	return false
}

// ensureVault opens (or returns the cached) vault. Errors are sticky: once
// vault open fails, all subsequent ops return the same error. This is
// deliberate — a vault open failure means the host's security posture is
// wrong and the operator must fix it before DB ops can resume.
func (h *Handler) ensureVault() (*Vault, error) {
	h.once.Do(func() {
		h.vault, h.err = OpenVault()
	})
	return h.vault, h.err
}

// ---------- ops ----------

func (h *Handler) opDiscover(ctx context.Context, args []string) *Result {
	req := &pb.DatabaseDiscoverRequest{}
	if len(args) >= 2 && args[1] != "" {
		if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
			return errResult(fmt.Sprintf("discover: unmarshal request: %v", err))
		}
	}
	resp, err := Discover(ctx, req)
	if err != nil {
		return errResult(fmt.Sprintf("discover: %v", err))
	}
	return marshalOK(resp, "discover")
}

func (h *Handler) opConnect(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("connect: missing JSON request in args[1]")
	}
	req := &pb.DatabaseConnectRequest{}
	if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
		return errResult(fmt.Sprintf("connect: unmarshal request: %v", err))
	}
	if req.GetInstanceId() == "" {
		return errResult("connect: instance_id is required")
	}
	if req.GetConnection() == nil {
		return errResult("connect: connection is required")
	}

	vault, err := h.ensureVault()
	if err != nil {
		return errResult(fmt.Sprintf("connect: vault: %v", err))
	}

	driver, err := DriverFor(req.GetEngine())
	if err != nil {
		return errResult(fmt.Sprintf("connect: %v", err))
	}

	// Probe with the supplied creds — never persist a credential we have
	// not verified can authenticate.
	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := driver.Open(openCtx, req.GetConnection(), req.GetUsername(), req.GetPassword())
	if err != nil {
		return errResult(fmt.Sprintf("connect: open: %v", err))
	}
	defer func() { _ = db.Close() }()

	verCtx, vcancel := context.WithTimeout(ctx, 5*time.Second)
	defer vcancel()
	ver, err := driver.Version(verCtx, db)
	if err != nil {
		return errResult(fmt.Sprintf("connect: version: %v", err))
	}

	entry := &CredEntry{
		Engine:        engineEnumToString(req.GetEngine()),
		Host:          req.GetConnection().GetHost(),
		Port:          req.GetConnection().GetPort(),
		SocketPath:    req.GetConnection().GetSocketPath(),
		Username:      req.GetUsername(),
		Password:      req.GetPassword(),
		UseTLS:        req.GetConnection().GetUseTls(),
		TLSCAPem:      req.GetConnection().GetTlsCaPem(),
		DiscoveryHint: req.GetDiscoveryHint(),
		ConnectedAt:   time.Now().UTC(),
	}
	if err := vault.Put(req.GetInstanceId(), entry); err != nil {
		return errResult(fmt.Sprintf("connect: vault put: %v", err))
	}

	return marshalOK(&pb.DatabaseConnectResponse{
		Success:       true,
		Message:       fmt.Sprintf("connected to %s", req.GetInstanceId()),
		ServerVersion: ver,
	}, "connect")
}

func (h *Handler) opDisconnect(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("disconnect: missing JSON request in args[1]")
	}
	ref := &pb.DatabaseInstanceRef{}
	if err := protojson.Unmarshal([]byte(args[1]), ref); err != nil {
		return errResult(fmt.Sprintf("disconnect: unmarshal request: %v", err))
	}
	if ref.GetInstanceId() == "" {
		return errResult("disconnect: instance_id is required")
	}
	vault, err := h.ensureVault()
	if err != nil {
		return errResult(fmt.Sprintf("disconnect: vault: %v", err))
	}
	if err := vault.Delete(ref.GetInstanceId()); err != nil {
		return errResult(fmt.Sprintf("disconnect: vault delete: %v", err))
	}
	return marshalOK(&pb.DatabaseDisconnectResponse{Success: true}, "disconnect")
}

func (h *Handler) opPing(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("ping: missing JSON request in args[1]")
	}
	ref := &pb.DatabaseInstanceRef{}
	if err := protojson.Unmarshal([]byte(args[1]), ref); err != nil {
		return errResult(fmt.Sprintf("ping: unmarshal request: %v", err))
	}
	if ref.GetInstanceId() == "" {
		return errResult("ping: instance_id is required")
	}

	vault, err := h.ensureVault()
	if err != nil {
		return errResult(fmt.Sprintf("ping: vault: %v", err))
	}
	entry, err := vault.Get(ref.GetInstanceId())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errResult(fmt.Sprintf("ping: unknown instance %q (not in vault)", ref.GetInstanceId()))
		}
		return errResult(fmt.Sprintf("ping: vault get: %v", err))
	}

	engine, ok := engineStringToEnum(entry.Engine)
	if !ok {
		return errResult(fmt.Sprintf("ping: unknown engine %q in vault", entry.Engine))
	}
	driver, err := DriverFor(engine)
	if err != nil {
		return errResult(fmt.Sprintf("ping: %v", err))
	}

	conn := &pb.DatabaseConnection{
		Host:       entry.Host,
		Port:       entry.Port,
		SocketPath: entry.SocketPath,
		UseTls:     entry.UseTLS,
		TlsCaPem:   entry.TLSCAPem,
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	start := time.Now()
	db, err := driver.Open(pingCtx, conn, entry.Username, entry.Password)
	if err != nil {
		return errResult(fmt.Sprintf("ping: open: %v", err))
	}
	defer func() { _ = db.Close() }()

	verCtx, vcancel := context.WithTimeout(ctx, 5*time.Second)
	defer vcancel()
	ver, err := driver.Version(verCtx, db)
	if err != nil {
		return errResult(fmt.Sprintf("ping: version: %v", err))
	}
	elapsed := time.Since(start)

	return marshalOK(&pb.DatabasePingResponse{
		Reachable:     true,
		LatencyMs:     uint64(elapsed.Milliseconds()),
		ServerVersion: ver,
	}, "ping")
}

// ---------- helpers ----------

func engineEnumToString(e pb.DatabaseEngine) string {
	switch e {
	case pb.DatabaseEngine_DATABASE_ENGINE_MYSQL:
		return "mysql"
	case pb.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		return "mariadb"
	case pb.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		return "postgresql"
	default:
		return ""
	}
}

func engineStringToEnum(s string) (pb.DatabaseEngine, bool) {
	switch s {
	case "mysql":
		return pb.DatabaseEngine_DATABASE_ENGINE_MYSQL, true
	case "mariadb":
		return pb.DatabaseEngine_DATABASE_ENGINE_MARIADB, true
	case "postgresql":
		return pb.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, true
	default:
		return pb.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, false
	}
}

// marshalOK encodes msg as protojson and returns it as a successful Result.
// On marshal failure the op name is included in the error to make the
// failure self-describing in the agent log.
func marshalOK(msg proto.Message, op string) *Result {
	b, err := protojson.Marshal(msg)
	if err != nil {
		return errResult(fmt.Sprintf("%s: marshal response: %v", op, err))
	}
	return &Result{ExitCode: 0, Stdout: string(b)}
}

// protoResult is the convenience entry point used by the PR-3 ops, which
// don't need the per-op label that marshalOK builds into its error.
func protoResult(msg proto.Message) *Result {
	b, err := protojson.Marshal(msg)
	if err != nil {
		return errResult(fmt.Sprintf("marshal response: %v", err))
	}
	return &Result{ExitCode: 0, Stdout: string(b)}
}

func errResult(msg string) *Result {
	return &Result{ExitCode: 1, Stderr: msg}
}

// ---------- PR 3 dispatch wrappers ----------
//
// Each wrapper unmarshals args[1] into the right pb.*Request, delegates
// to the engine-aware op*, and serializes the response (or surfaces the
// error). Destructive ops verify the confirmation token before delegating.

func (h *Handler) opListDBsCmd(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("list_dbs: missing JSON request in args[1]")
	}
	req := &pb.DatabaseInstanceRef{}
	if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
		return errResult(fmt.Sprintf("list_dbs: unmarshal request: %v", err))
	}
	resp, err := h.opListDBs(ctx, req)
	if err != nil {
		return errResult(fmt.Sprintf("list_dbs: %v", err))
	}
	return protoResult(resp)
}

func (h *Handler) opListUsersCmd(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("list_users: missing JSON request in args[1]")
	}
	req := &pb.DatabaseInstanceRef{}
	if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
		return errResult(fmt.Sprintf("list_users: unmarshal request: %v", err))
	}
	resp, err := h.opListUsers(ctx, req)
	if err != nil {
		return errResult(fmt.Sprintf("list_users: %v", err))
	}
	return protoResult(resp)
}

func (h *Handler) opCreateDBCmd(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("create_db: missing JSON request in args[1]")
	}
	req := &pb.DatabaseCreateDBRequest{}
	if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
		return errResult(fmt.Sprintf("create_db: unmarshal request: %v", err))
	}
	resp, err := h.opCreateDB(ctx, req)
	if err != nil {
		return errResult(fmt.Sprintf("create_db: %v", err))
	}
	return protoResult(resp)
}

func (h *Handler) opCreateUserCmd(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("create_user: missing JSON request in args[1]")
	}
	req := &pb.DatabaseCreateUserRequest{}
	if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
		return errResult(fmt.Sprintf("create_user: unmarshal request: %v", err))
	}
	resp, err := h.opCreateUser(ctx, req)
	if err != nil {
		return errResult(fmt.Sprintf("create_user: %v", err))
	}
	return protoResult(resp)
}

func (h *Handler) opGrantCmd(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("grant: missing JSON request in args[1]")
	}
	req := &pb.DatabaseGrantRequest{}
	if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
		return errResult(fmt.Sprintf("grant: unmarshal request: %v", err))
	}
	resp, err := h.opGrant(ctx, req)
	if err != nil {
		return errResult(fmt.Sprintf("grant: %v", err))
	}
	return protoResult(resp)
}

func (h *Handler) opDropDBCmd(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("drop_db: missing JSON request in args[1]")
	}
	req := &pb.DatabaseDropDBRequest{}
	if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
		return errResult(fmt.Sprintf("drop_db: unmarshal request: %v", err))
	}
	if err := verifyOpToken(
		req.GetConfirmationToken(),
		"drop_db",
		req.GetInstance().GetInstanceId(),
		req.GetDatabaseName(),
	); err != nil {
		return errResult(fmt.Sprintf("drop_db: token verification failed: %v", err))
	}
	resp, err := h.opDropDB(ctx, req)
	if err != nil {
		return errResult(fmt.Sprintf("drop_db: %v", err))
	}
	return protoResult(resp)
}

func (h *Handler) opDropUserCmd(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("drop_user: missing JSON request in args[1]")
	}
	req := &pb.DatabaseDropUserRequest{}
	if err := protojson.Unmarshal([]byte(args[1]), req); err != nil {
		return errResult(fmt.Sprintf("drop_user: unmarshal request: %v", err))
	}
	host := req.GetHost()
	if host == "" {
		host = defaultMySQLHost
	}
	target := fmt.Sprintf("%s@%s", req.GetUsername(), host)
	if err := verifyOpToken(
		req.GetConfirmationToken(),
		"drop_user",
		req.GetInstance().GetInstanceId(),
		target,
	); err != nil {
		return errResult(fmt.Sprintf("drop_user: token verification failed: %v", err))
	}
	resp, err := h.opDropUser(ctx, req)
	if err != nil {
		return errResult(fmt.Sprintf("drop_user: %v", err))
	}
	return protoResult(resp)
}
