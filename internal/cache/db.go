package cache

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Core routing decision persisted per store path.
type RouteEntry struct {
	StorePath    string
	UpstreamURL  string
	LatencyMs    float64
	LatencyEMA   float64
	LastVerified time.Time
	QueryCount   uint32
	FailureCount uint32
	TTL          time.Time
	NarHash      string
	NarSize      uint64
}

// Returns true if the entry exists and hasn't expired.
func (r *RouteEntry) IsValid() bool {
	return r != nil && time.Now().Before(r.TTL)
}

// SQLite-backed store for route persistence.
type DB struct {
	db         *sql.DB
	maxEntries int
}

// Opens or creates the SQLite database at path with WAL mode.
func Open(path string, maxEntries int) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_journal=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite WAL allows 1 writer

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &DB{db: db, maxEntries: maxEntries}, nil
}

// Closes the database.
func (d *DB) Close() error {
	return d.db.Close()
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS routes (
			store_path     TEXT PRIMARY KEY,
			upstream_url   TEXT NOT NULL,
			latency_ms     REAL DEFAULT 0,
			latency_ema    REAL DEFAULT 0,
			query_count    INTEGER DEFAULT 1,
			failure_count  INTEGER DEFAULT 0,
			last_verified  INTEGER DEFAULT 0,
			ttl            INTEGER NOT NULL,
			nar_hash       TEXT DEFAULT '',
			nar_size       INTEGER DEFAULT 0,
			created_at     INTEGER DEFAULT (strftime('%s', 'now'))
		);
		CREATE INDEX IF NOT EXISTS idx_routes_ttl ON routes(ttl);
		CREATE INDEX IF NOT EXISTS idx_routes_last_verified ON routes(last_verified);

		CREATE TABLE IF NOT EXISTS upstream_health (
			url                TEXT PRIMARY KEY,
			ema_latency        REAL DEFAULT 0,
			last_probe         INTEGER DEFAULT 0,
			consecutive_fails  INTEGER DEFAULT 0,
			total_queries      INTEGER DEFAULT 0,
			success_rate       REAL DEFAULT 1.0
		);
		CREATE TABLE IF NOT EXISTS negative_cache (
			store_path TEXT PRIMARY KEY,
			expires_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_negative_expires ON negative_cache(expires_at);
	`)
	return err
}

// Returns the route for storePath, or nil if not found.
func (d *DB) GetRoute(storePath string) (*RouteEntry, error) {
	row := d.db.QueryRow(`
		SELECT store_path, upstream_url, latency_ms, latency_ema,
		       query_count, failure_count, last_verified, ttl, nar_hash, nar_size
		FROM routes WHERE store_path = ?`, storePath)

	var e RouteEntry
	var lastVerifiedUnix, ttlUnix int64
	err := row.Scan(
		&e.StorePath, &e.UpstreamURL, &e.LatencyMs, &e.LatencyEMA,
		&e.QueryCount, &e.FailureCount, &lastVerifiedUnix, &ttlUnix,
		&e.NarHash, &e.NarSize,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.LastVerified = time.Unix(lastVerifiedUnix, 0).UTC()
	e.TTL = time.Unix(ttlUnix, 0).UTC()
	return &e, nil
}

// Inserts or updates a route entry.
func (d *DB) SetRoute(entry *RouteEntry) error {
	_, err := d.db.Exec(`
		INSERT INTO routes
			(store_path, upstream_url, latency_ms, latency_ema,
			 query_count, failure_count, last_verified, ttl, nar_hash, nar_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(store_path) DO UPDATE SET
			upstream_url  = excluded.upstream_url,
			latency_ms    = excluded.latency_ms,
			latency_ema   = excluded.latency_ema,
			query_count   = excluded.query_count,
			failure_count = excluded.failure_count,
			last_verified = excluded.last_verified,
			ttl           = excluded.ttl,
			nar_hash      = excluded.nar_hash,
			nar_size      = excluded.nar_size`,
		entry.StorePath, entry.UpstreamURL,
		entry.LatencyMs, entry.LatencyEMA,
		entry.QueryCount, entry.FailureCount,
		entry.LastVerified.Unix(), entry.TTL.Unix(),
		entry.NarHash, entry.NarSize,
	)
	if err != nil {
		return err
	}
	return d.evictIfNeeded()
}

// Deletes routes whose TTL has passed.
func (d *DB) ExpireOldRoutes() error {
	_, err := d.db.Exec(`DELETE FROM routes WHERE ttl < ?`, time.Now().Unix())
	return err
}

// Returns up to n non-expired routes ordered by most-recently-verified.
func (d *DB) ListRecentRoutes(n int) ([]RouteEntry, error) {
	rows, err := d.db.Query(`
		SELECT store_path, upstream_url, latency_ema, last_verified, ttl, nar_hash, nar_size
		FROM routes WHERE ttl > ? ORDER BY last_verified DESC LIMIT ?`,
		time.Now().Unix(), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RouteEntry
	for rows.Next() {
		var e RouteEntry
		var lastVerifiedUnix, ttlUnix int64
		if err := rows.Scan(
			&e.StorePath, &e.UpstreamURL, &e.LatencyEMA,
			&lastVerifiedUnix, &ttlUnix, &e.NarHash, &e.NarSize,
		); err != nil {
			return nil, err
		}
		e.LastVerified = time.Unix(lastVerifiedUnix, 0).UTC()
		e.TTL = time.Unix(ttlUnix, 0).UTC()
		result = append(result, e)
	}
	return result, rows.Err()
}

// Returns the total number of stored routes.
func (d *DB) RouteCount() (int, error) {
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM routes`).Scan(&count)
	return count, err
}

// Records a negative cache entry for storePath with the given TTL.
func (d *DB) SetNegative(storePath string, ttl time.Duration) error {
	_, err := d.db.Exec(
		`INSERT INTO negative_cache (store_path, expires_at) VALUES (?, ?)
		 ON CONFLICT(store_path) DO UPDATE SET expires_at = excluded.expires_at`,
		storePath, time.Now().Add(ttl).Unix(),
	)
	return err
}

// Returns true if a non-expired negative entry exists for storePath.
func (d *DB) IsNegative(storePath string) (bool, error) {
	var count int
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM negative_cache WHERE store_path = ? AND expires_at > ?`,
		storePath, time.Now().Unix(),
	).Scan(&count)
	return count > 0, err
}

// Deletes expired negative cache entries.
func (d *DB) ExpireNegatives() error {
	_, err := d.db.Exec(`DELETE FROM negative_cache WHERE expires_at < ?`, time.Now().Unix())
	return err
}

// Deletes the oldest routes (by last_verified) when over capacity.
func (d *DB) evictIfNeeded() error {
	count, err := d.RouteCount()
	if err != nil {
		return err
	}
	if count <= d.maxEntries {
		return nil
	}
	excess := count - d.maxEntries
	_, err = d.db.Exec(`
		DELETE FROM routes WHERE store_path IN (
			SELECT store_path FROM routes ORDER BY last_verified ASC LIMIT ?
		)`, excess)
	return err
}
