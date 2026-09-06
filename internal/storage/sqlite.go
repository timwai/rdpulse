package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrEnrollmentInvalid  = errors.New("enrollment invitation is invalid, expired, or already consumed")
	ErrInvitationNotFound = errors.New("enrollment invitation not found")
)

// AgentRecord represents a registered agent in the database
type AgentRecord struct {
	ID         int64
	DeviceID   string
	Hostname   string
	SecretHash string
	PublicPort uint16
	Enabled    bool
	CreatedAt  time.Time
	LastSeen   time.Time
}

// ConnectionLog records a completed TCP or UDP connection
type ConnectionLog struct {
	ID         int64
	DeviceID   string
	Protocol   string
	SourceIP   string
	SourcePort int
	StartTime  time.Time
	EndTime    time.Time
	RxBytes    int64
	TxBytes    int64
}

// EnrollmentInvitationRecord represents an invitation entry in storage
type EnrollmentInvitationRecord struct {
	DeviceID   string     `json:"deviceId"`
	TokenHash  string     `json:"tokenHash"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	ConsumedAt *time.Time `json:"consumedAt,omitempty"`
}

// DB encapsulates the SQLite database connection
type DB struct {
	db *sql.DB
}

// CreateAgent enrolls a device exactly once. A duplicate DeviceID returns an
// error instead of replacing the credential chosen by the first registrant.
func (d *DB) CreateAgent(a *AgentRecord) error {
	enabled := 0
	if a.Enabled {
		enabled = 1
	}
	_, err := d.db.Exec(`
		INSERT INTO agents (device_id, hostname, secret_hash, public_port, enabled, created_at, last_seen)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, a.DeviceID, a.Hostname, a.SecretHash, a.PublicPort, enabled)
	return err
}

// Open initializes the SQLite database with WAL mode and creates required tables
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create db directory failed: %w", err)
		}
	}

	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=5000", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite failed: %w", err)
	}

	// In WAL mode with _busy_timeout=5000, concurrent reads are safe and parallelized.
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	db := &DB{db: sqlDB}
	if err := db.initSchema(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	return db, nil
}

func (d *DB) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS agents (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		device_id TEXT UNIQUE NOT NULL,
		hostname TEXT,
		secret_hash TEXT NOT NULL,
		public_port INTEGER NOT NULL,
		enabled INTEGER DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen DATETIME
	);

	CREATE TABLE IF NOT EXISTS connection_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		device_id TEXT NOT NULL,
		protocol TEXT NOT NULL,
		source_ip TEXT NOT NULL,
		source_port INTEGER NOT NULL,
		start_time DATETIME NOT NULL,
		end_time DATETIME NOT NULL,
		rx_bytes INTEGER NOT NULL,
		tx_bytes INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS enrollment_invitations (
		device_id TEXT PRIMARY KEY,
		token_hash TEXT NOT NULL,
		expires_at DATETIME NOT NULL,
		consumed_at DATETIME
	);

	CREATE INDEX IF NOT EXISTS idx_agents_device_id ON agents(device_id);
	CREATE INDEX IF NOT EXISTS idx_agents_public_port ON agents(public_port);
	`
	_, err := d.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("execute schema failed: %w", err)
	}
	return nil
}

// UpsertEnrollmentInvitation installs or rotates a device-scoped invitation.
// Restarting with the same token preserves its consumed state; changing the
// token explicitly rotates it and makes the new invitation available once.
func (d *DB) UpsertEnrollmentInvitation(deviceID, token string, expiresAt time.Time) error {
	_, err := d.db.Exec(`
		INSERT INTO enrollment_invitations (device_id, token_hash, expires_at, consumed_at)
		VALUES (?, ?, ?, NULL)
		ON CONFLICT(device_id) DO UPDATE SET
			expires_at = excluded.expires_at,
			consumed_at = CASE
				WHEN enrollment_invitations.token_hash = excluded.token_hash THEN enrollment_invitations.consumed_at
				ELSE NULL
			END,
			token_hash = excluded.token_hash
	`, deviceID, enrollmentTokenHash(token), expiresAt.UTC())
	return err
}

// UpdateEnrollmentInvitationExpiry changes only the expiry, keeping the token and consumed state.
func (d *DB) UpdateEnrollmentInvitationExpiry(deviceID string, expiresAt time.Time) error {
	res, err := d.db.Exec(`
		UPDATE enrollment_invitations SET expires_at = ? WHERE device_id = ?
	`, expiresAt.UTC(), deviceID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInvitationNotFound
	}
	return nil
}

func (d *DB) ValidateEnrollmentInvitation(deviceID, token string, now time.Time) (bool, error) {
	if deviceID == "" || token == "" {
		return false, nil
	}
	var found int
	err := d.db.QueryRow(`
		SELECT 1 FROM enrollment_invitations
		WHERE device_id = ? AND token_hash = ? AND consumed_at IS NULL AND expires_at > ?
	`, deviceID, enrollmentTokenHash(token), now.UTC()).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && found == 1, err
}

// CreateAgentWithEnrollment atomically consumes an invitation and creates its
// bound device. Concurrent reuse cannot replace or create another credential.
func (d *DB) CreateAgentWithEnrollment(a *AgentRecord, token string, now time.Time) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`
		UPDATE enrollment_invitations SET consumed_at = ?
		WHERE device_id = ? AND token_hash = ? AND consumed_at IS NULL AND expires_at > ?
	`, now.UTC(), a.DeviceID, enrollmentTokenHash(token), now.UTC())
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ErrEnrollmentInvalid
	}
	enabled := 0
	if a.Enabled {
		enabled = 1
	}
	if _, err := tx.Exec(`
		INSERT INTO agents (device_id, hostname, secret_hash, public_port, enabled, created_at, last_seen)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(device_id) DO UPDATE SET
			hostname = excluded.hostname,
			secret_hash = excluded.secret_hash,
			public_port = CASE WHEN agents.public_port > 0 THEN agents.public_port ELSE excluded.public_port END,
			enabled = 1,
			last_seen = CURRENT_TIMESTAMP
	`, a.DeviceID, a.Hostname, a.SecretHash, a.PublicPort, enabled); err != nil {
		return err
	}
	return tx.Commit()
}

func enrollmentTokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

// GetAgentByDeviceID returns agent record for the specified device ID
func (d *DB) GetAgentByDeviceID(deviceID string) (*AgentRecord, error) {
	row := d.db.QueryRow(
		"SELECT id, device_id, hostname, secret_hash, public_port, enabled, created_at, last_seen FROM agents WHERE device_id = ?",
		deviceID,
	)

	var a AgentRecord
	var enabledInt int
	var lastSeen sql.NullTime

	err := row.Scan(&a.ID, &a.DeviceID, &a.Hostname, &a.SecretHash, &a.PublicPort, &enabledInt, &a.CreatedAt, &lastSeen)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	a.Enabled = enabledInt == 1
	if lastSeen.Valid {
		a.LastSeen = lastSeen.Time
	}
	return &a, nil
}

// GetAllAgents retrieves all configured agents
func (d *DB) GetAllAgents() ([]*AgentRecord, error) {
	rows, err := d.db.Query("SELECT id, device_id, hostname, secret_hash, public_port, enabled, created_at, last_seen FROM agents")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*AgentRecord
	for rows.Next() {
		var a AgentRecord
		var enabledInt int
		var lastSeen sql.NullTime
		if err := rows.Scan(&a.ID, &a.DeviceID, &a.Hostname, &a.SecretHash, &a.PublicPort, &enabledInt, &a.CreatedAt, &lastSeen); err != nil {
			return nil, err
		}
		a.Enabled = enabledInt == 1
		if lastSeen.Valid {
			a.LastSeen = lastSeen.Time
		}
		results = append(results, &a)
	}
	return results, rows.Err()
}

// UpsertAgent creates or updates an agent
func (d *DB) UpsertAgent(a *AgentRecord) error {
	enabled := 0
	if a.Enabled {
		enabled = 1
	}

	query := `
	INSERT INTO agents (device_id, hostname, secret_hash, public_port, enabled, created_at, last_seen)
	VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	ON CONFLICT(device_id) DO UPDATE SET
		hostname = excluded.hostname,
		secret_hash = excluded.secret_hash,
		public_port = excluded.public_port,
		enabled = excluded.enabled,
		last_seen = CURRENT_TIMESTAMP;
	`
	_, err := d.db.Exec(query, a.DeviceID, a.Hostname, a.SecretHash, a.PublicPort, enabled)
	return err
}

// UpdateAgentRegistration refreshes mutable runtime registration fields
// without changing the stored credential or administrative enabled flag.
func (d *DB) UpdateAgentRegistration(deviceID, hostname string, publicPort uint16) error {
	_, err := d.db.Exec(
		"UPDATE agents SET hostname = ?, public_port = ?, last_seen = CURRENT_TIMESTAMP WHERE device_id = ?",
		hostname,
		publicPort,
		deviceID,
	)
	return err
}

// UpdateAgentLastSeen updates the last_seen timestamp
func (d *DB) UpdateAgentLastSeen(deviceID string, t time.Time) error {
	_, err := d.db.Exec("UPDATE agents SET last_seen = ? WHERE device_id = ?", t, deviceID)
	return err
}

// LogConnection writes connection stats record
func (d *DB) LogConnection(log *ConnectionLog) error {
	query := `
	INSERT INTO connection_log (device_id, protocol, source_ip, source_port, start_time, end_time, rx_bytes, tx_bytes)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := d.db.Exec(query, log.DeviceID, log.Protocol, log.SourceIP, log.SourcePort, log.StartTime, log.EndTime, log.RxBytes, log.TxBytes)
	return err
}

// ListEnrollmentInvitations retrieves all enrollment invitations
func (d *DB) ListEnrollmentInvitations() ([]*EnrollmentInvitationRecord, error) {
	rows, err := d.db.Query("SELECT device_id, token_hash, expires_at, consumed_at FROM enrollment_invitations ORDER BY expires_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []*EnrollmentInvitationRecord
	for rows.Next() {
		var rec EnrollmentInvitationRecord
		var consumed sql.NullTime
		if err := rows.Scan(&rec.DeviceID, &rec.TokenHash, &rec.ExpiresAt, &consumed); err != nil {
			return nil, err
		}
		if consumed.Valid {
			t := consumed.Time
			rec.ConsumedAt = &t
		}
		list = append(list, &rec)
	}
	return list, rows.Err()
}

// DeleteEnrollmentInvitation deletes an invitation by deviceID
func (d *DB) DeleteEnrollmentInvitation(deviceID string) error {
	_, err := d.db.Exec("DELETE FROM enrollment_invitations WHERE device_id = ?", deviceID)
	return err
}

// DeleteAgent deletes an agent by deviceID
func (d *DB) DeleteAgent(deviceID string) error {
	_, err := d.db.Exec("DELETE FROM agents WHERE device_id = ?", deviceID)
	return err
}

// SetAgentEnabled enables or disables an agent
func (d *DB) SetAgentEnabled(deviceID string, enabled bool) error {
	val := 0
	if enabled {
		val = 1
	}
	_, err := d.db.Exec("UPDATE agents SET enabled = ? WHERE device_id = ?", val, deviceID)
	return err
}

// CleanupOldLogs deletes connection logs older than the specified retention days
func (d *DB) CleanupOldLogs(retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	res, err := d.db.Exec("DELETE FROM connection_log WHERE start_time < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Close closes the database connection
func (d *DB) Close() error {
	return d.db.Close()
}
