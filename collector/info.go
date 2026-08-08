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
//
// constLabels traz os metadados de integração com o PMM (service_name, cluster,
// environment, replication_set — ver mariadb_exporter_pmm_integration.md, seção
// 3). São ConstLabels, não labels dinâmicos: diferem dos quatro labels normais
// de mariadb_info por não variarem por linha de resultado, apenas por instância
// do exporter.
func NewInfoCollector(logger log.Logger, features FeatureProvider, constLabels prometheus.Labels) *InfoCollector {
	return &InfoCollector{
		base: newBase("info", "Informações de versão e identificação da instância MariaDB.", true, logger, features),
		desc: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, "", "info"),
			"Informações da instância MariaDB; o valor é sempre 1.",
			[]string{"version", "version_comment", "hostname", "server_id"},
			constLabels,
		),
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
		sanitizeLabel(version.String), sanitizeLabel(comment.String),
		sanitizeLabel(hostname.String), sanitizeLabel(serverID.String),
	)
	return nil
}
