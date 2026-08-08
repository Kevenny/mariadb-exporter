package collector

import (
	"context"
	"database/sql"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// userStatQuery reads information_schema.USER_STATISTICS.
//
// The table has no ROWS_CHANGED column: MariaDB breaks it down into
// ROWS_DELETED, ROWS_INSERTED and ROWS_UPDATED (unlike TABLE_STATISTICS, which
// has ROWS_CHANGED). The sum of the three reproduces the "rows changed"
// semantics requested by the specification.
const userStatQuery = `
SELECT USER,
       TOTAL_CONNECTIONS,
       CONCURRENT_CONNECTIONS,
       ROWS_READ,
       ROWS_SENT,
       ROWS_DELETED + ROWS_INSERTED + ROWS_UPDATED AS ROWS_CHANGED,
       SELECT_COMMANDS,
       UPDATE_COMMANDS,
       OTHER_COMMANDS,
       ACCESS_DENIED,
       LOST_CONNECTIONS
FROM information_schema.USER_STATISTICS`

// UserStatCollector collects information_schema.USER_STATISTICS.
// Requires `SET GLOBAL userstat = ON`.
type UserStatCollector struct {
	base

	totalConnections      *prometheus.Desc
	concurrentConnections *prometheus.Desc
	rowsRead              *prometheus.Desc
	rowsSent              *prometheus.Desc
	rowsChanged           *prometheus.Desc
	selectCommands        *prometheus.Desc
	updateCommands        *prometheus.Desc
	otherCommands         *prometheus.Desc
	accessDenied          *prometheus.Desc
	lostConnections       *prometheus.Desc
}

// NewUserStatCollector creates the userstat collector.
func NewUserStatCollector(enabled bool, logger log.Logger, features FeatureProvider) *UserStatCollector {
	labels := []string{"user"}
	return &UserStatCollector{
		base: newBase("userstat", "Per-user statistics from information_schema.USER_STATISTICS (requires userstat=ON).", enabled, logger, features),

		totalConnections:      newDesc("user", "total_connections_total", "Total connections made by the user.", labels),
		concurrentConnections: newDesc("user", "concurrent_connections", "Current concurrent connections for the user.", labels),
		rowsRead:              newDesc("user", "rows_read_total", "Total rows read by the user.", labels),
		rowsSent:              newDesc("user", "rows_sent_total", "Total rows sent to the user.", labels),
		rowsChanged:           newDesc("user", "rows_changed_total", "Total rows changed by the user.", labels),
		selectCommands:        newDesc("user", "select_commands_total", "Total SELECT commands executed by the user.", labels),
		updateCommands:        newDesc("user", "update_commands_total", "Total UPDATE commands executed by the user.", labels),
		otherCommands:         newDesc("user", "other_commands_total", "Total other commands executed by the user.", labels),
		accessDenied:          newDesc("user", "access_denied_total", "Total access denied errors for the user.", labels),
		lostConnections:       newDesc("user", "lost_connections_total", "Total lost connections for the user.", labels),
	}
}

// Available implements Availability: depends on the userstat variable.
func (c *UserStatCollector) Available() bool {
	return c.featureFlags().HasUserStat
}

// Collect implements Collector.
func (c *UserStatCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "userstat is OFF; no metrics will be collected. Enable with SET GLOBAL userstat = ON")
		return nil
	}

	rows, err := db.QueryContext(ctx, userStatQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			user                              sql.NullString
			totalConn, concurrentConn         sql.NullFloat64
			rowsRead, rowsSent, rowsChanged   sql.NullFloat64
			selectCmds, updateCmds, otherCmds sql.NullFloat64
			accessDenied, lostConn            sql.NullFloat64
		)

		if err := rows.Scan(
			&user, &totalConn, &concurrentConn,
			&rowsRead, &rowsSent, &rowsChanged,
			&selectCmds, &updateCmds, &otherCmds,
			&accessDenied, &lostConn,
		); err != nil {
			return err
		}

		u := user.String

		emitCounter(ch, c.totalConnections, totalConn, u)
		emitGauge(ch, c.concurrentConnections, concurrentConn, u)
		emitCounter(ch, c.rowsRead, rowsRead, u)
		emitCounter(ch, c.rowsSent, rowsSent, u)
		emitCounter(ch, c.rowsChanged, rowsChanged, u)
		emitCounter(ch, c.selectCommands, selectCmds, u)
		emitCounter(ch, c.updateCommands, updateCmds, u)
		emitCounter(ch, c.otherCommands, otherCmds, u)
		emitCounter(ch, c.accessDenied, accessDenied, u)
		emitCounter(ch, c.lostConnections, lostConn, u)
	}

	return rows.Err()
}

// emitCounter sends a counter only if the value is not NULL. NULL values are
// omitted instead of becoming zero, to avoid inventing data the server did
// not provide.
//
// The labels go through sanitizeLabels: values coming from the database might
// not be valid UTF-8 and would make client_golang panic, bringing down the
// exporter.
func emitCounter(ch chan<- prometheus.Metric, desc *prometheus.Desc, v sql.NullFloat64, labels ...string) {
	if !v.Valid {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, v.Float64, sanitizeLabels(labels)...)
}

// emitGauge sends a gauge only if the value is not NULL.
func emitGauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, v sql.NullFloat64, labels ...string) {
	if !v.Valid {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v.Float64, sanitizeLabels(labels)...)
}
