// Package exporter contains the collector orchestrator and the detection of
// MariaDB version and plugins.
package exporter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/Kevenny/mariadb-exporter/collector"
)

// FeatureRefreshInterval is the interval of the asynchronous plugin refresh
// (section 9 of the specification).
const FeatureRefreshInterval = 5 * time.Minute

// ErrNotMariaDB is returned when the connected instance is not a MariaDB.
var ErrNotMariaDB = errors.New("instance is not MariaDB")

// versionRe extracts major.minor.patch from the start of the VERSION()
// string. Covers formats like "11.4.3-MariaDB",
// "10.11.6-MariaDB-1:10.11.6+maria~ubu2204" and "5.5.5-10.6.12-MariaDB"
// (compatibility prefix, handled beforehand).
var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

// mysqlCompatPrefix is the prefix that MariaDB advertises to legacy clients
// when the version variable has the compatibility replicate-annotate. In
// those cases the real version comes after the prefix.
const mysqlCompatPrefix = "5.5.5-"

// DetectVersion runs SELECT VERSION() and @@version_comment, confirms that
// the instance is MariaDB, and extracts major.minor.patch.
//
// Per section 2.1 of the specification, an instance that is not MariaDB is a
// fatal startup error: it returns ErrNotMariaDB wrapped in a descriptive
// message.
func DetectVersion(ctx context.Context, db *sql.DB) (*collector.VersionInfo, error) {
	var full, comment string

	// version_comment is optional: on some forks/proxies it doesn't exist,
	// and its absence must not prevent detection.
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&full); err != nil {
		return nil, fmt.Errorf("failed to run SELECT VERSION(): %w", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT @@global.version_comment").Scan(&comment); err != nil {
		comment = ""
	}

	info := ParseVersion(full, comment)
	if !info.IsMariaDB {
		return info, fmt.Errorf(
			"%w: VERSION() returned %q (version_comment: %q). "+
				"mariadb_exporter collects metrics exclusive to MariaDB "+
				"(USER_STATISTICS, QUERY_RESPONSE_TIME, METADATA_LOCK_INFO, DISKS, SHOW ALL SLAVES STATUS) "+
				"and does not work against MySQL. For MySQL use mysqld_exporter",
			ErrNotMariaDB, full, comment,
		)
	}

	return info, nil
}

// ParseVersion parses the VERSION() string without needing a connection,
// which makes testing easier.
func ParseVersion(full, comment string) *collector.VersionInfo {
	info := &collector.VersionInfo{
		Full:    full,
		Comment: comment,
	}

	haystack := strings.ToLower(full + " " + comment)
	info.IsMariaDB = strings.Contains(haystack, "mariadb")

	numeric := strings.TrimSpace(full)
	// MariaDB may prefix the version with "5.5.5-" for legacy clients; the
	// true version comes after this prefix.
	if strings.HasPrefix(numeric, mysqlCompatPrefix) {
		numeric = strings.TrimPrefix(numeric, mysqlCompatPrefix)
	}

	if m := versionRe.FindStringSubmatch(numeric); m != nil {
		info.Major, _ = strconv.Atoi(m[1])
		info.Minor, _ = strconv.Atoi(m[2])
		info.Patch, _ = strconv.Atoi(m[3])
	}

	return info
}

// FeatureDetector detects and keeps the instance's feature flags up to date.
// Implements collector.FeatureProvider and is safe for concurrent use.
type FeatureDetector struct {
	db     *sql.DB
	logger log.Logger

	mu       sync.RWMutex
	version  *collector.VersionInfo
	features *collector.FeatureFlags
}

// NewFeatureDetector creates the detector with the version already known.
func NewFeatureDetector(db *sql.DB, version *collector.VersionInfo, logger log.Logger) *FeatureDetector {
	return &FeatureDetector{
		db:       db,
		logger:   logger,
		version:  version,
		features: &collector.FeatureFlags{},
	}
}

// Version returns the detected version.
func (d *FeatureDetector) Version() *collector.VersionInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.version
}

// Features returns a copy of the current feature flags.
func (d *FeatureDetector) Features() *collector.FeatureFlags {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.features == nil {
		return &collector.FeatureFlags{}
	}
	copied := *d.features
	return &copied
}

// pluginQuery queries the plugins relevant to the exporter (section 9).
const pluginQuery = `
SELECT plugin_name, plugin_status
FROM information_schema.plugins
WHERE plugin_name IN (
    'QUERY_RESPONSE_TIME',
    'QUERY_RESPONSE_TIME_AUDIT',
    'METADATA_LOCK_INFO',
    'DISKS'
)`

const userStatQuery = `
SELECT VARIABLE_VALUE
FROM information_schema.GLOBAL_VARIABLES
WHERE VARIABLE_NAME = 'userstat'`

// Refresh re-queries plugins and variables, updating the feature flags.
//
// Individual errors are logged but do not abort the refresh: a missing
// permission for one of the queries should not zero out the other features.
func (d *FeatureDetector) Refresh(ctx context.Context) {
	features := &collector.FeatureFlags{}

	if err := d.detectPlugins(ctx, features); err != nil {
		_ = level.Warn(d.logger).Log("msg", "failed to detect plugins", "err", err)
	}
	if err := d.detectUserStat(ctx, features); err != nil {
		_ = level.Warn(d.logger).Log("msg", "failed to detect userstat", "err", err)
	}
	if err := d.detectGalera(ctx, features); err != nil {
		_ = level.Warn(d.logger).Log("msg", "failed to detect wsrep/galera", "err", err)
	}
	if err := d.detectReplica(ctx, features); err != nil {
		_ = level.Warn(d.logger).Log("msg", "failed to detect replica state", "err", err)
	}

	d.mu.Lock()
	d.features = features
	d.mu.Unlock()

	_ = level.Debug(d.logger).Log(
		"msg", "feature flags updated",
		"userstat", features.HasUserStat,
		"query_response_time", features.HasQueryResponseTime,
		"metadata_lock_info", features.HasMetadataLockInfo,
		"disks", features.HasDisksPlugin,
		"galera", features.HasGalera,
		"replica", features.IsReplica,
	)
}

func (d *FeatureDetector) detectPlugins(ctx context.Context, features *collector.FeatureFlags) error {
	rows, err := d.db.QueryContext(ctx, pluginQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var name, status string
		if err := rows.Scan(&name, &status); err != nil {
			return err
		}
		if !strings.EqualFold(status, "ACTIVE") {
			continue
		}
		switch strings.ToUpper(name) {
		case "QUERY_RESPONSE_TIME", "QUERY_RESPONSE_TIME_AUDIT":
			features.HasQueryResponseTime = true
		case "METADATA_LOCK_INFO":
			features.HasMetadataLockInfo = true
		case "DISKS":
			features.HasDisksPlugin = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// The installed plugin only delivers data if collection is turned on;
	// without that the QUERY_RESPONSE_TIME table exists but stays empty.
	if features.HasQueryResponseTime {
		var value string
		err := d.db.QueryRowContext(ctx, "SELECT @@global.query_response_time_stats").Scan(&value)
		switch {
		case err != nil:
			// Missing variable: keeps the plugin as available and lets the
			// collector decide based on the table's content.
			_ = level.Debug(d.logger).Log("msg", "query_response_time_stats unavailable", "err", err)
		case !isTruthy(value):
			features.HasQueryResponseTime = false
		}
	}

	// The minimum version for METADATA_LOCK_INFO is 10.0.7 (section 2.2).
	if features.HasMetadataLockInfo {
		if v := d.Version(); v != nil && !v.AtLeast(10, 0, 7) {
			_ = level.Warn(d.logger).Log(
				"msg", "METADATA_LOCK_INFO requires MariaDB >= 10.0.7; collector will be disabled",
				"version", v.String(),
			)
			features.HasMetadataLockInfo = false
		}
	}

	return nil
}

func (d *FeatureDetector) detectUserStat(ctx context.Context, features *collector.FeatureFlags) error {
	var value string
	if err := d.db.QueryRowContext(ctx, userStatQuery).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	features.HasUserStat = isTruthy(value)
	return nil
}

func (d *FeatureDetector) detectGalera(ctx context.Context, features *collector.FeatureFlags) error {
	var name, value string
	err := d.db.QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE 'wsrep_ready'").Scan(&name, &value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	features.HasGalera = isTruthy(value)
	return nil
}

// detectReplica checks whether the instance replicates from some master.
// Uses gtid_slave_pos as the primary signal since it is cheap and does not
// require the REPLICATION CLIENT privilege.
func (d *FeatureDetector) detectReplica(ctx context.Context, features *collector.FeatureFlags) error {
	var value string
	err := d.db.QueryRowContext(ctx, "SELECT @@global.gtid_slave_pos").Scan(&value)
	if err == nil && strings.TrimSpace(value) != "" {
		features.IsReplica = true
		return nil
	}

	// Fallback: counts the rows from SHOW ALL SLAVES STATUS. Absence of rows
	// means the instance is not a replica.
	rows, qErr := d.db.QueryContext(ctx, "SHOW ALL SLAVES STATUS")
	if qErr != nil {
		if err != nil {
			return err
		}
		return nil
	}
	defer rows.Close()
	features.IsReplica = rows.Next()
	return rows.Err()
}

// Run triggers an immediate refresh and then one every FeatureRefreshInterval
// until the context is canceled.
func (d *FeatureDetector) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = FeatureRefreshInterval
	}

	d.Refresh(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Refresh(ctx)
		}
	}
}

// isTruthy interprets the textual boolean values used by MariaDB.
func isTruthy(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "ON", "1", "YES", "TRUE", "ALL", "ACTIVE":
		return true
	default:
		return false
	}
}
