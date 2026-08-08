package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// ReplicationCollector collects replica state.
//
// MariaDB supports native multi-source replication, so the primary source is
// SHOW ALL SLAVES STATUS, which returns one row per replication connection.
// On very old servers (or builds without the command) there is a fallback to
// SHOW SLAVE STATUS, which returns at most one row (section 2.2, replication).
type ReplicationCollector struct {
	base

	sqlRunning          *prometheus.Desc
	ioRunning           *prometheus.Desc
	secondsBehindMaster *prometheus.Desc
	lastErrno           *prometheus.Desc
	relayLogPos         *prometheus.Desc
}

// NewReplicationCollector creates the replication collector.
func NewReplicationCollector(enabled bool, logger log.Logger, features FeatureProvider) *ReplicationCollector {
	labels := []string{"connection_name", "master_host", "master_port"}
	return &ReplicationCollector{
		base: newBase("replication", "Replication state via SHOW ALL SLAVES STATUS (supports multi-source).", enabled, logger, features),

		sqlRunning:          newDesc("slave", "sql_running", "1 if the replica's SQL thread is running, 0 otherwise.", labels),
		ioRunning:           newDesc("slave", "io_running", "1 if the replica's I/O thread is running, 0 otherwise.", labels),
		secondsBehindMaster: newDesc("slave", "seconds_behind_master", "Replica lag relative to the master, in seconds.", labels),
		lastErrno:           newDesc("slave", "last_errno", "Code of the replica's last error (0 = no error).", labels),
		relayLogPos:         newDesc("slave", "relay_log_pos", "Current position in the relay log.", labels),
	}
}

// Collect implements Collector.
func (c *ReplicationCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	rows, err := db.QueryContext(ctx, "SHOW ALL SLAVES STATUS")
	if err != nil {
		_ = level.Debug(c.Logger()).Log("msg", "SHOW ALL SLAVES STATUS unavailable, using SHOW SLAVE STATUS", "err", err)

		rows, err = db.QueryContext(ctx, "SHOW SLAVE STATUS")
		if err != nil {
			return err
		}
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return err
	}

	// The column set varies quite a bit across MariaDB versions, so reading is
	// done into a map indexed by the lowercase column name instead of fixed
	// positions.
	for rows.Next() {
		values := make([]sql.RawBytes, len(columns))
		scanArgs := make([]interface{}, len(columns))
		for i := range values {
			scanArgs[i] = &values[i]
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return err
		}

		row := make(map[string]string, len(columns))
		for i, col := range columns {
			row[strings.ToLower(col)] = string(values[i])
		}

		// Without Connection_name (single-source or fallback), the label is
		// left empty, which is the default connection identifier in MariaDB.
		connectionName := row["connection_name"]
		masterHost := row["master_host"]
		masterPort := row["master_port"]

		labels := []string{connectionName, masterHost, masterPort}

		emitBool(ch, c.sqlRunning, row["slave_sql_running"], labels...)
		emitBool(ch, c.ioRunning, row["slave_io_running"], labels...)

		// Seconds_Behind_Master is NULL when replication is stopped; in that
		// case the metric is omitted instead of reporting 0, which would be
		// read as "replica up to date".
		emitNumeric(ch, c.secondsBehindMaster, row["seconds_behind_master"], prometheus.GaugeValue, labels...)
		emitNumeric(ch, c.lastErrno, row["last_errno"], prometheus.GaugeValue, labels...)
		emitNumeric(ch, c.relayLogPos, row["relay_log_pos"], prometheus.GaugeValue, labels...)
	}

	// Zero rows means the instance is not a replica: no metric and no error
	// (section 20, replication with 0, 1 or N slaves).
	return rows.Err()
}

// emitBool converts Yes/No/ON/OFF to 1/0.
func emitBool(ch chan<- prometheus.Metric, desc *prometheus.Desc, raw string, labels ...string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}

	var value float64
	switch strings.ToUpper(raw) {
	case "YES", "ON", "1", "TRUE":
		value = 1
	case "NO", "OFF", "0", "FALSE":
		value = 0
	case "CONNECTING":
		// Connecting is not "running": the thread exists but is not yet
		// replicating.
		value = 0
	default:
		return
	}

	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, sanitizeLabels(labels)...)
}

// emitNumeric sends the value if it is numeric; empty strings and NULL are
// silently ignored.
func emitNumeric(ch chan<- prometheus.Metric, desc *prometheus.Desc, raw string, kind prometheus.ValueType, labels ...string) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "NULL") {
		return
	}

	v, err := parseFloat(raw)
	if err != nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(desc, kind, v, sanitizeLabels(labels)...)
}
