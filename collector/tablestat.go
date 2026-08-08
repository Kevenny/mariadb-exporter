package collector

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// tableStatQueryTmpl orders by ROWS_READ DESC before applying the limit, so
// the cutoff preserves the busiest tables (section 2.2, tablestat).
const tableStatQueryTmpl = `
SELECT TABLE_SCHEMA,
       TABLE_NAME,
       ROWS_READ,
       ROWS_CHANGED,
       ROWS_CHANGED_X_INDEXES
FROM information_schema.TABLE_STATISTICS
ORDER BY ROWS_READ DESC
LIMIT %d`

// TableStatCollector collects information_schema.TABLE_STATISTICS.
// Requires `SET GLOBAL userstat = ON`.
type TableStatCollector struct {
	base
	limit int

	rowsRead            *prometheus.Desc
	rowsChanged         *prometheus.Desc
	rowsChangedXIndexes *prometheus.Desc
}

// NewTableStatCollector creates the tablestat collector with the limit of
// tables per scrape.
func NewTableStatCollector(enabled bool, limit int, logger log.Logger, features FeatureProvider) *TableStatCollector {
	labels := []string{"schema", "table"}
	return &TableStatCollector{
		base:  newBase("tablestat", "Per-table statistics from information_schema.TABLE_STATISTICS (requires userstat=ON).", enabled, logger, features),
		limit: limit,

		rowsRead:            newDesc("table", "rows_read_total", "Total rows read from the table.", labels),
		rowsChanged:         newDesc("table", "rows_changed_total", "Total rows changed in the table.", labels),
		rowsChangedXIndexes: newDesc("table", "rows_changed_x_indexes_total", "Total rows changed multiplied by the number of affected indexes.", labels),
	}
}

// Available implements Availability: depends on the userstat variable.
func (c *TableStatCollector) Available() bool {
	return c.featureFlags().HasUserStat
}

// Collect implements Collector.
func (c *TableStatCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "userstat is OFF; no metrics will be collected. Enable with SET GLOBAL userstat = ON")
		return nil
	}

	limit := c.limit
	if limit <= 0 {
		limit = 500
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf(tableStatQueryTmpl, limit))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			schema, table                          sql.NullString
			rowsRead, rowsChanged, rowsChangedXIdx sql.NullFloat64
		)

		if err := rows.Scan(&schema, &table, &rowsRead, &rowsChanged, &rowsChangedXIdx); err != nil {
			return err
		}

		s, t := schema.String, table.String

		emitCounter(ch, c.rowsRead, rowsRead, s, t)
		emitCounter(ch, c.rowsChanged, rowsChanged, s, t)
		emitCounter(ch, c.rowsChangedXIndexes, rowsChangedXIdx, s, t)
	}

	return rows.Err()
}
