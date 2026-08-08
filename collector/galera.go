package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// GaleraCollector expõe métricas do cluster Galera a partir das variáveis
// wsrep_*. Coletor opt-in, habilitado via --collector.galera.
type GaleraCollector struct {
	base

	clusterSize       *prometheus.Desc
	clusterStatus     *prometheus.Desc
	localState        *prometheus.Desc
	flowControlPaused *prometheus.Desc
	recvQueueAvg      *prometheus.Desc
	sendQueueAvg      *prometheus.Desc
}

// NewGaleraCollector cria o coletor galera.
func NewGaleraCollector(enabled bool, logger log.Logger, features FeatureProvider) *GaleraCollector {
	labels := []string{"cluster_name"}
	return &GaleraCollector{
		base: newBase("galera", "Métricas do cluster Galera a partir de SHOW STATUS LIKE 'wsrep_%' (opt-in).", enabled, logger, features),

		clusterSize:       newDesc("galera", "cluster_size", "Número de nós que compõem o cluster.", labels),
		clusterStatus:     newDesc("galera", "cluster_status", "Estado do componente do cluster: 1=Primary, 0=non-Primary.", labels),
		localState:        newDesc("galera", "local_state", "Estado local do nó: 0=joining, 1=donor/desynced, 2=joined, 3=synced.", labels),
		flowControlPaused: newDesc("galera", "flow_control_paused", "Fração do tempo em que a replicação ficou pausada por flow control.", labels),
		recvQueueAvg:      newDesc("galera", "recv_queue_avg", "Tamanho médio da fila de recebimento desde a última consulta.", labels),
		sendQueueAvg:      newDesc("galera", "send_queue_avg", "Tamanho médio da fila de envio desde a última consulta.", labels),
	}
}

// Available implementa Availability: depende do wsrep estar ativo.
func (c *GaleraCollector) Available() bool {
	return c.featureFlags().HasGalera
}

// Collect implementa Collector.
func (c *GaleraCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "wsrep/Galera inativo nesta instância; nenhuma métrica será coletada")
		return nil
	}

	rows, err := db.QueryContext(ctx, "SHOW GLOBAL STATUS LIKE 'wsrep_%'")
	if err != nil {
		return err
	}
	defer rows.Close()

	// O nome do cluster é um label comum a todas as métricas, mas chega como uma
	// linha do mesmo result set — por isso as variáveis são coletadas primeiro e
	// as métricas emitidas depois.
	values := make(map[string]string)

	for rows.Next() {
		var name string
		var raw sql.RawBytes

		if err := rows.Scan(&name, &raw); err != nil {
			return err
		}
		values[strings.ToLower(name)] = string(raw)
	}

	if err := rows.Err(); err != nil {
		return err
	}

	clusterName := values["wsrep_cluster_name"]
	if clusterName == "" {
		// wsrep_cluster_name é uma variável de configuração, não de status; se não
		// vier no SHOW STATUS, busca direto.
		var v sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT @@global.wsrep_cluster_name").Scan(&v); err == nil {
			clusterName = v.String
		}
	}

	// O nome do cluster vira label em todas as métricas deste coletor; se vier
	// com bytes inválidos, sanitizar aqui cobre todos os pontos de emissão.
	clusterName = sanitizeLabel(clusterName)

	emit := func(desc *prometheus.Desc, variable string) {
		raw, ok := values[variable]
		if !ok {
			return
		}
		v, err := parseFloat(raw)
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "variável wsrep ignorada", "variavel", variable, "valor", raw, "err", err)
			return
		}
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, clusterName)
	}

	emit(c.clusterSize, "wsrep_cluster_size")
	emit(c.flowControlPaused, "wsrep_flow_control_paused")
	emit(c.recvQueueAvg, "wsrep_local_recv_queue_avg")
	emit(c.sendQueueAvg, "wsrep_local_send_queue_avg")

	// wsrep_cluster_status é textual ("Primary"/"non-Primary").
	if raw, ok := values["wsrep_cluster_status"]; ok {
		value := 0.0
		if strings.EqualFold(strings.TrimSpace(raw), "primary") {
			value = 1
		}
		ch <- prometheus.MustNewConstMetric(c.clusterStatus, prometheus.GaugeValue, value, clusterName)
	}

	// wsrep_local_state já é numérico no protocolo do Galera, porém a numeração
	// nativa é 1=joining, 2=donor/desynced, 3=joined, 4=synced. A especificação
	// pede 0=joining, 1=donor, 2=joined, 3=synced, então é subtraído 1.
	if raw, ok := values["wsrep_local_state"]; ok {
		if v, err := parseFloat(raw); err == nil {
			ch <- prometheus.MustNewConstMetric(c.localState, prometheus.GaugeValue, v-1, clusterName)
		}
	}

	return nil
}
