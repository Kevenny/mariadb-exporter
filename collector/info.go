package collector

import (
	"context"
	"database/sql"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

const infoQuery = `SELECT VERSION(), @@global.version_comment, @@global.hostname, @@global.server_id`

// InfoCollector expõe mariadb_info, uma info metric com valor sempre 1 cujos
// labels descrevem a instância. Sempre habilitado (seção 2.2, coletor info).
type InfoCollector struct {
	base
	desc *prometheus.Desc
}

// NewInfoCollector cria o coletor info. Ele não recebe flag de habilitação
// porque, por especificação, não pode ser desabilitado.
func NewInfoCollector(logger log.Logger, features FeatureProvider) *InfoCollector {
	return &InfoCollector{
		base: newBase("info", "Informações de versão e identificação da instância MariaDB.", true, logger, features),
		desc: newDesc("", "info", "Informações da instância MariaDB; o valor é sempre 1.",
			[]string{"version", "version_comment", "hostname", "server_id"}),
	}
}

// Collect implementa Collector.
func (c *InfoCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	var version, comment, hostname, serverID sql.NullString

	if err := db.QueryRowContext(ctx, infoQuery).Scan(&version, &comment, &hostname, &serverID); err != nil {
		return err
	}

	ch <- prometheus.MustNewConstMetric(
		c.desc, prometheus.GaugeValue, 1,
		version.String, comment.String, hostname.String, serverID.String,
	)
	return nil
}
