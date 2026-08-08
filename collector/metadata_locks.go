package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// metadataLockQuery reads the metadata_lock_info plugin. The TABLE_SCHEMA and
// TABLE_NAME columns are empty for locks that are not table-scoped (e.g.
// GLOBAL, SCHEMA).
const metadataLockQuery = `
SELECT LOCK_MODE, LOCK_TYPE, TABLE_SCHEMA, TABLE_NAME
FROM information_schema.METADATA_LOCK_INFO`

// MetadataLocksCollector aggregates active metadata locks by mode, type and
// table. Requires the metadata_lock_info plugin and MariaDB >= 10.0.7.
type MetadataLocksCollector struct {
	base

	total   *prometheus.Desc
	waiting *prometheus.Desc
}

// NewMetadataLocksCollector creates the metadata_locks collector.
func NewMetadataLocksCollector(enabled bool, logger log.Logger, features FeatureProvider) *MetadataLocksCollector {
	labels := []string{"lock_mode", "lock_type", "table_schema", "table_name"}
	return &MetadataLocksCollector{
		base: newBase("metadata_locks",
			"Active metadata locks from information_schema.METADATA_LOCK_INFO (requires the metadata_lock_info plugin and MariaDB >= 10.0.7).",
			enabled, logger, features),

		total:   newDesc("metadata_locks", "total", "Total active metadata locks grouped by mode, type and table.", labels),
		waiting: newDesc("metadata_lock", "waiting_total", "Total waiting metadata locks grouped by mode, type and table.", labels),
	}
}

// Available implements Availability: depends on the metadata_lock_info plugin
// and the minimum version, both already resolved by the feature detector.
func (c *MetadataLocksCollector) Available() bool {
	return c.featureFlags().HasMetadataLockInfo
}

// Collect implements Collector.
func (c *MetadataLocksCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "metadata_lock_info plugin inactive (or MariaDB < 10.0.7); no metrics will be collected")
		return nil
	}

	rows, err := db.QueryContext(ctx, metadataLockQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Aggregates in memory: the table lists one lock per row, and what matters
	// is the count per label combination.
	type key struct {
		mode, lockType, schema, table string
	}
	totals := make(map[key]float64)
	waits := make(map[key]float64)

	for rows.Next() {
		var mode, lockType, schema, table sql.NullString

		if err := rows.Scan(&mode, &lockType, &schema, &table); err != nil {
			return err
		}

		k := key{
			mode:     mode.String,
			lockType: lockType.String,
			schema:   schema.String,
			table:    table.String,
		}

		totals[k]++

		// Pending locks appear with LOCK_MODE containing "WAIT" (e.g. a waiting
		// MDL_SHARED_WRITE turns into a waiting state reported in the mode).
		if strings.Contains(strings.ToUpper(k.mode), "WAIT") {
			waits[k]++
		} else if _, seen := waits[k]; !seen {
			// Ensures the waiting series exists with 0 when there are active
			// locks for the same combination, avoiding gaps in the graph.
			waits[k] = 0
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}

	for k, v := range totals {
		ch <- prometheus.MustNewConstMetric(
			c.total, prometheus.GaugeValue, v,
			sanitizeLabel(k.mode), sanitizeLabel(k.lockType),
			sanitizeLabel(k.schema), sanitizeLabel(k.table),
		)
	}
	for k, v := range waits {
		ch <- prometheus.MustNewConstMetric(
			c.waiting, prometheus.GaugeValue, v,
			sanitizeLabel(k.mode), sanitizeLabel(k.lockType),
			sanitizeLabel(k.schema), sanitizeLabel(k.table),
		)
	}

	return nil
}
