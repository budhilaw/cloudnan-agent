// Package database — ops_provision.go.
//
// One-shot operation that fixes the most common MySQL/MariaDB
// "mysqldump: 1698: Access denied for user 'root'@'localhost'"
// gotcha by minting a dedicated dump-only user, granting the
// privileges mysqldump actually needs, and rotating the vault entry
// to use those credentials. From that point on `export_dump` calls
// the new user instead of root, so socket-auth-by-default
// configurations (Debian / Ubuntu MariaDB) stop blocking exports.
//
// Why this is safe to run on a working instance: it only ADDS a new
// user, it doesn't touch root. If the new user already exists the
// op rotates its password (idempotent). The previous credential
// stays valid in the database until the operator drops it.
package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	pb "github.com/cloudnan-tech/cloudnan-agent/proto/agent"
)

// dumpUserName is fixed by the agent — the panel surfaces it in the
// UI so the operator knows which DB user the panel is using. Using
// a stable name keeps the user idempotent across re-provisions and
// makes audit-log filters straightforward.
const dumpUserName = "cloudnan_dump"

// provisionDumpUserRequest is the JSON shape sent in args[1]. We use
// JSON instead of regenerating the protobuf so the agent can ship
// independently of a control-plane proto bump.
type provisionDumpUserRequest struct {
	InstanceID string `json:"instance_id"`
}

type provisionDumpUserResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Username string `json:"username"`
}

// opProvisionDumpUserCmd is the args-shape entry point dispatched
// from handler.go. Mirrors the other opXxxCmd helpers.
func (h *Handler) opProvisionDumpUserCmd(ctx context.Context, args []string) *Result {
	if len(args) < 2 {
		return errResult("provision_dump_user: missing JSON request in args[1]")
	}
	req := &provisionDumpUserRequest{}
	if err := json.Unmarshal([]byte(args[1]), req); err != nil {
		return errResult(fmt.Sprintf("provision_dump_user: unmarshal: %v", err))
	}
	if strings.TrimSpace(req.InstanceID) == "" {
		return errResult("provision_dump_user: instance_id is required")
	}
	resp, err := h.opProvisionDumpUser(ctx, req)
	if err != nil {
		return errResult(fmt.Sprintf("provision_dump_user: %v", err))
	}
	out, _ := json.Marshal(resp)
	return &Result{ExitCode: 0, Stdout: string(out)}
}

// opProvisionDumpUser performs the privilege-grant + vault rotation.
// Postgres has no equivalent gotcha (peer auth uses the OS user too,
// but pg_dump-as-postgres just works when run by root via sudo
// switching) so the op is a no-op on Postgres.
func (h *Handler) opProvisionDumpUser(ctx context.Context, req *provisionDumpUserRequest) (*provisionDumpUserResponse, error) {
	driver, db, entry, err := h.openInstance(ctx, req.InstanceID)
	if err != nil {
		// The caller commonly hits this op when the existing
		// credentials don't work (1698). Try a socket fallback as
		// root with no password — that's what unix_socket / auth_socket
		// allows when the agent runs as OS root. If that also fails,
		// surface a clear message.
		driver2, db2, entry2, sockErr := h.openInstanceViaSocket(ctx, req.InstanceID)
		if sockErr != nil {
			return nil, fmt.Errorf("open instance: %w (socket fallback also failed: %v)", err, sockErr)
		}
		driver, db, entry = driver2, db2, entry2
	}
	defer func() { _ = db.Close() }()

	switch driver.Engine() {
	case pb.DatabaseEngine_DATABASE_ENGINE_MYSQL,
		pb.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		// fall through to the MySQL path below
	case pb.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		return &provisionDumpUserResponse{
			Success:  true,
			Message:  "postgres exports use peer auth — no dedicated dump user needed",
			Username: entry.Username,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported engine %v", driver.Engine())
	}

	password, err := generateStrongPassword(32)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}

	// CREATE OR ALTER — `CREATE USER IF NOT EXISTS` is supported on
	// MariaDB 10.1+ and MySQL 5.7.6+, both well below our support
	// floor. The ALTER ensures we rotate the password even when the
	// user already exists from a previous provision.
	createStmt := fmt.Sprintf(
		"CREATE USER IF NOT EXISTS '%s'@'localhost' IDENTIFIED BY '%s'",
		escapeSQLString(dumpUserName), escapeSQLString(password),
	)
	if _, err := db.ExecContext(ctx, createStmt); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	alterStmt := fmt.Sprintf(
		"ALTER USER '%s'@'localhost' IDENTIFIED BY '%s'",
		escapeSQLString(dumpUserName), escapeSQLString(password),
	)
	if _, err := db.ExecContext(ctx, alterStmt); err != nil {
		return nil, fmt.Errorf("alter user (rotate password): %w", err)
	}

	// Privilege set required for mysqldump --single-transaction
	// --routines --events --triggers across every database on the
	// instance. Read-only — explicit list, no GRANT ALL.
	grantStmt := fmt.Sprintf(
		"GRANT SELECT, LOCK TABLES, SHOW VIEW, EVENT, TRIGGER, RELOAD, REPLICATION CLIENT "+
			"ON *.* TO '%s'@'localhost'",
		escapeSQLString(dumpUserName),
	)
	if _, err := db.ExecContext(ctx, grantStmt); err != nil {
		return nil, fmt.Errorf("grant privileges: %w", err)
	}
	if _, err := db.ExecContext(ctx, "FLUSH PRIVILEGES"); err != nil {
		return nil, fmt.Errorf("flush privileges: %w", err)
	}

	// Verify the new credentials before persisting them — never
	// rotate the vault to a credential we haven't proven works.
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	verifyConn := &pb.DatabaseConnection{
		Host:       entry.Host,
		Port:       entry.Port,
		SocketPath: entry.SocketPath,
		UseTls:     entry.UseTLS,
		TlsCaPem:   entry.TLSCAPem,
	}
	verifyDB, err := driver.Open(verifyCtx, verifyConn, dumpUserName, password)
	if err != nil {
		return nil, fmt.Errorf("verify new credentials: %w", err)
	}
	if err := verifyDB.PingContext(verifyCtx); err != nil {
		_ = verifyDB.Close()
		return nil, fmt.Errorf("verify ping: %w", err)
	}
	_ = verifyDB.Close()

	// Rotate the vault entry. Keep the host / port / TLS / discovery
	// hint metadata as-is; only username + password change.
	vault, err := h.ensureVault()
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	newEntry := *entry
	newEntry.Username = dumpUserName
	newEntry.Password = password
	newEntry.ConnectedAt = time.Now().UTC()
	if err := vault.Put(req.InstanceID, &newEntry); err != nil {
		return nil, fmt.Errorf("vault put: %w", err)
	}

	return &provisionDumpUserResponse{
		Success:  true,
		Message:  fmt.Sprintf("provisioned %s@localhost with read-only export privileges", dumpUserName),
		Username: dumpUserName,
	}, nil
}

// openInstanceViaSocket attempts the unix_socket auth fallback used
// when the vault-stored credentials don't authenticate (typically
// the Debian/Ubuntu default where root has only the unix_socket
// plugin and password=anything is rejected). The agent runs as OS
// root, so connecting via the local socket as user 'root' with no
// password is exactly what the unix_socket plugin checks for.
func (h *Handler) openInstanceViaSocket(ctx context.Context, instanceID string) (Driver, *sql.DB, *CredEntry, error) {
	driver, entry, _, err := h.resolveInstance(instanceID)
	if err != nil {
		return nil, nil, nil, err
	}
	if entry.SocketPath == "" {
		// Best-effort guess at the canonical socket paths. We try
		// the well-known locations and pick the first one that
		// exists. If none, bail.
		for _, candidate := range []string{
			"/var/run/mysqld/mysqld.sock", // Debian/Ubuntu
			"/run/mysqld/mysqld.sock",
			"/tmp/mysql.sock",       // common upstream default
			"/var/lib/mysql/mysql.sock", // RHEL family
		} {
			if _, sErr := net.DialTimeout("unix", candidate, 500*time.Millisecond); sErr == nil {
				entry.SocketPath = candidate
				break
			}
		}
		if entry.SocketPath == "" {
			return nil, nil, nil, errors.New("no MySQL/MariaDB unix socket found in canonical locations")
		}
	}
	conn := &pb.DatabaseConnection{
		SocketPath: entry.SocketPath,
		UseTls:     entry.UseTLS,
		TlsCaPem:   entry.TLSCAPem,
	}
	db, err := driver.Open(ctx, conn, "root", "")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open via socket as root: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, nil, nil, fmt.Errorf("socket ping as root: %w", err)
	}
	return driver, db, entry, nil
}

// generateStrongPassword returns a URL-safe base64 password of the
// requested byte length (post-encoding the string is roughly 4/3 the
// byte count). 32 bytes → 43 chars of entropy = far stronger than
// anything mysqldump's command line will ever hold.
func generateStrongPassword(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
