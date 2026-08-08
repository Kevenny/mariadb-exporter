package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// InnoDBCollector exposes InnoDB engine metrics.
//
// The specification cites SHOW ENGINE INNODB STATUS as the source, but the
// requested values (buffer pool, row locks, deadlocks) are all available as
// counters in SHOW GLOBAL STATUS, which is a structured format that's stable
// across versions — parsing INNODB STATUS's free-form text would be fragile.
// Deadlocks are the exception: they only exist in the text, so there is a
// fallback for it.
type InnoDBCollector struct {
	base

	bufferPoolReadRequests *prometheus.Desc
	bufferPoolReads        *prometheus.Desc
	bufferPoolPages        *prometheus.Desc
	rowLockWaits           *prometheus.Desc
	rowLockTimeAvg         *prometheus.Desc
	deadlocks              *prometheus.Desc
}

// NewInnoDBCollector creates the innodb collector.
func NewInnoDBCollector(enabled bool, logger log.Logger, features FeatureProvider) *InnoDBCollector {
	return &InnoDBCollector{
		base: newBase("innodb", "InnoDB engine metrics (buffer pool, row locks, deadlocks).", enabled, logger, features),

		bufferPoolReadRequests: newDesc("innodb", "buffer_pool_read_requests_total", "Total logical reads requested from the buffer pool.", nil),
		bufferPoolReads:        newDesc("innodb", "buffer_pool_reads_total", "Total reads the buffer pool could not satisfy and went to disk.", nil),
		bufferPoolPages:        newDesc("innodb", "buffer_pool_pages_total", "Buffer pool pages by type.", []string{"type"}),
		rowLockWaits:           newDesc("innodb", "row_lock_waits_total", "Total times an operation waited for a row lock.", nil),
		rowLockTimeAvg:         newDesc("innodb", "row_lock_time_avg_milliseconds", "Average row lock wait time in milliseconds.", nil),
		deadlocks:              newDesc("innodb", "deadlocks_total", "Total deadlocks detected by InnoDB.", nil),
	}
}

// innodbStatusMappings links SHOW GLOBAL STATUS variables to this collector's
// simple (label-less) metrics.
var innodbStatusMappings = map[string]struct {
	field string
	kind  prometheus.ValueType
}{
	"innodb_buffer_pool_read_requests": {"bufferPoolReadRequests", prometheus.CounterValue},
	"innodb_buffer_pool_reads":         {"bufferPoolReads", prometheus.CounterValue},
	"innodb_row_lock_waits":            {"rowLockWaits", prometheus.CounterValue},
	"innodb_row_lock_time_avg":         {"rowLockTimeAvg", prometheus.GaugeValue},
	"innodb_deadlocks":                 {"deadlocks", prometheus.CounterValue},
}

// innodbPageTypes maps the buffer pool page variables to the `type` label
// value (section 2.2: free, data, dirty).
var innodbPageTypes = map[string]string{
	"innodb_buffer_pool_pages_free":  "free",
	"innodb_buffer_pool_pages_data":  "data",
	"innodb_buffer_pool_pages_dirty": "dirty",
	"innodb_buffer_pool_pages_misc":  "misc",
	"innodb_buffer_pool_pages_total": "total",
}

// Collect implements Collector.
func (c *InnoDBCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL STATUS LIKE 'Innodb_%'")
	if err != nil {
		return err
	}
	defer rows.Close()

	descByField := map[string]*prometheus.Desc{
		"bufferPoolReadRequests": c.bufferPoolReadRequests,
		"bufferPoolReads":        c.bufferPoolReads,
		"rowLockWaits":           c.rowLockWaits,
		"rowLockTimeAvg":         c.rowLockTimeAvg,
		"deadlocks":              c.deadlocks,
	}

	sawDeadlocks := false

	for rows.Next() {
		var name string
		var raw sql.RawBytes

		if err := rows.Scan(&name, &raw); err != nil {
			return err
		}

		key := strings.ToLower(name)

		if mapping, ok := innodbStatusMappings[key]; ok {
			v, err := parseFloat(string(raw))
			if err != nil {
				_ = level.Debug(c.Logger()).Log("msg", "InnoDB variable ignored", "variable", name, "err", err)
				continue
			}
			ch <- prometheus.MustNewConstMetric(descByField[mapping.field], mapping.kind, v)
			if key == "innodb_deadlocks" {
				sawDeadlocks = true
			}
			continue
		}

		if pageType, ok := innodbPageTypes[key]; ok {
			v, err := parseFloat(string(raw))
			if err != nil {
				_ = level.Debug(c.Logger()).Log("msg", "InnoDB page variable ignored", "variable", name, "err", err)
				continue
			}
			ch <- prometheus.MustNewConstMetric(c.bufferPoolPages, prometheus.GaugeValue, v, pageType)
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}

	// Innodb_deadlocks does not exist on all builds (it's an XtraDB/Percona
	// status present in MariaDB but absent on some versions). When missing,
	// falls back to the counter from SHOW ENGINE INNODB STATUS's text.
	if !sawDeadlocks {
		if count, ok := c.deadlockCountFromEngineStatus(ctx, db); ok {
			ch <- prometheus.MustNewConstMetric(c.deadlocks, prometheus.CounterValue, count)
		}
	}

	return nil
}

// deadlockCountFromEngineStatus extracts the deadlock count from the
// SHOW ENGINE INNODB STATUS text.
//
// The LATEST DETECTED DEADLOCK block does not carry a cumulative counter;
// what exists is the deadlock count in the TRANSACTIONS section on some
// builds. When nothing usable is found, it returns ok=false and no metric is
// emitted.
func (c *InnoDBCollector) deadlockCountFromEngineStatus(ctx context.Context, db *sql.DB) (float64, bool) {
	var engineType, name, status sql.NullString

	row := db.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS")
	if err := row.Scan(&engineType, &name, &status); err != nil {
		_ = level.Debug(c.Logger()).Log("msg", "SHOW ENGINE INNODB STATUS unavailable", "err", err)
		return 0, false
	}

	for _, line := range strings.Split(status.String, "\n") {
		trimmed := strings.TrimSpace(line)
		// Observed format: "Number of deadlocks 12"
		if !strings.HasPrefix(strings.ToLower(trimmed), "number of deadlocks") {
			continue
		}
		fields := strings.Fields(trimmed)
		v, err := parseFloat(fields[len(fields)-1])
		if err != nil {
			return 0, false
		}
		return v, true
	}

	return 0, false
}
