package collector

import (
	"context"
	"database/sql"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// diskQuery reads the DISKS plugin. The TOTAL, USED and AVAILABLE values come
// in kibibytes (1024 bytes) and are converted to bytes on exposure.
const diskQuery = `
SELECT Disk, Path, Total, Used, Available
FROM information_schema.DISKS`

// kibibyte is the conversion factor from the DISKS plugin values to bytes.
const kibibyte = 1024

// DisksCollector exposes filesystem usage as seen by the server.
// Requires the disks plugin.
type DisksCollector struct {
	base

	total     *prometheus.Desc
	used      *prometheus.Desc
	available *prometheus.Desc
}

// NewDisksCollector creates the disks collector.
func NewDisksCollector(enabled bool, logger log.Logger, features FeatureProvider) *DisksCollector {
	labels := []string{"disk", "path"}
	return &DisksCollector{
		base: newBase("disks", "Disk usage from information_schema.DISKS (requires the disks plugin).", enabled, logger, features),

		total:     newDesc("disk", "total_bytes", "Total filesystem capacity in bytes.", labels),
		used:      newDesc("disk", "used_bytes", "Bytes used on the filesystem.", labels),
		available: newDesc("disk", "available_bytes", "Bytes available on the filesystem.", labels),
	}
}

// Available implements Availability: depends on the disks plugin.
func (c *DisksCollector) Available() bool {
	return c.featureFlags().HasDisksPlugin
}

// Collect implements Collector.
func (c *DisksCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "disks plugin inactive; no metrics will be collected")
		return nil
	}

	rows, err := db.QueryContext(ctx, diskQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			disk, path             sql.NullString
			total, used, available sql.NullFloat64
		)

		if err := rows.Scan(&disk, &path, &total, &used, &available); err != nil {
			return err
		}

		d, p := disk.String, path.String

		emitGauge(ch, c.total, scaleKiB(total), d, p)
		emitGauge(ch, c.used, scaleKiB(used), d, p)
		emitGauge(ch, c.available, scaleKiB(available), d, p)
	}

	return rows.Err()
}

// scaleKiB converts a value in kibibytes to bytes, preserving the NULL state.
func scaleKiB(v sql.NullFloat64) sql.NullFloat64 {
	if !v.Valid {
		return v
	}
	return sql.NullFloat64{Float64: v.Float64 * kibibyte, Valid: true}
}
