package collector

import (
	"context"
	"database/sql"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

const infoQuery = `SELECT VERSION(), @@global.version_comment, @@global.hostname, @@global.server_id`

// InfoCollector exposes mariadb_info, an info metric whose value is always 1
// and whose labels describe the instance. Always enabled (section 2.2, info
// collector).
type InfoCollector struct {
	base
	desc *prometheus.Desc
}

// NewInfoCollector creates the info collector. It does not take an enable
// flag because, per the specification, it cannot be disabled.
//
// constLabels carries the PMM integration metadata (service_name, cluster,
// environment, replication_set — see mariadb_exporter_pmm_integration.md,
// section 3). These are ConstLabels, not dynamic labels: they differ from
// mariadb_info's four normal labels in that they don't vary per result row,
// only per exporter instance.
func NewInfoCollector(logger log.Logger, features FeatureProvider, constLabels prometheus.Labels) *InfoCollector {
	return &InfoCollector{
		base: newBase("info", "Version and identification information for the MariaDB instance.", true, logger, features),
		desc: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, "", "info"),
			"MariaDB instance information; the value is always 1.",
			[]string{"version", "version_comment", "hostname", "server_id"},
			constLabels,
		),
	}
}

// Collect implements Collector.
func (c *InfoCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	var version, comment, hostname, serverID sql.NullString

	if err := db.QueryRowContext(ctx, infoQuery).Scan(&version, &comment, &hostname, &serverID); err != nil {
		return err
	}

	ch <- prometheus.MustNewConstMetric(
		c.desc, prometheus.GaugeValue, 1,
		sanitizeLabel(version.String), sanitizeLabel(comment.String),
		sanitizeLabel(hostname.String), sanitizeLabel(serverID.String),
	)
	return nil
}
