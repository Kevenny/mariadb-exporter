package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// GaleraCollector exposes Galera cluster metrics from the wsrep_* variables.
// Opt-in collector, enabled via --collector.galera.
type GaleraCollector struct {
	base

	clusterSize       *prometheus.Desc
	clusterStatus     *prometheus.Desc
	localState        *prometheus.Desc
	flowControlPaused *prometheus.Desc
	recvQueueAvg      *prometheus.Desc
	sendQueueAvg      *prometheus.Desc
}

// NewGaleraCollector creates the galera collector.
func NewGaleraCollector(enabled bool, logger log.Logger, features FeatureProvider) *GaleraCollector {
	labels := []string{"cluster_name"}
	return &GaleraCollector{
		base: newBase("galera", "Galera cluster metrics from SHOW STATUS LIKE 'wsrep_%' (opt-in).", enabled, logger, features),

		clusterSize:       newDesc("galera", "cluster_size", "Number of nodes that make up the cluster.", labels),
		clusterStatus:     newDesc("galera", "cluster_status", "Cluster component state: 1=Primary, 0=non-Primary.", labels),
		localState:        newDesc("galera", "local_state", "Local node state: 0=joining, 1=donor/desynced, 2=joined, 3=synced.", labels),
		flowControlPaused: newDesc("galera", "flow_control_paused", "Fraction of time replication was paused due to flow control.", labels),
		recvQueueAvg:      newDesc("galera", "recv_queue_avg", "Average receive queue size since the last query.", labels),
		sendQueueAvg:      newDesc("galera", "send_queue_avg", "Average send queue size since the last query.", labels),
	}
}

// Available implements Availability: depends on wsrep being active.
func (c *GaleraCollector) Available() bool {
	return c.featureFlags().HasGalera
}

// Collect implements Collector.
func (c *GaleraCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "wsrep/Galera inactive on this instance; no metrics will be collected")
		return nil
	}

	rows, err := db.QueryContext(ctx, "SHOW GLOBAL STATUS LIKE 'wsrep_%'")
	if err != nil {
		return err
	}
	defer rows.Close()

	// The cluster name is a label common to all metrics, but it arrives as a
	// row of the same result set — that's why the variables are collected
	// first and the metrics emitted afterward.
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
		// wsrep_cluster_name is a configuration variable, not a status one; if
		// it doesn't come in SHOW STATUS, fetch it directly.
		var v sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT @@global.wsrep_cluster_name").Scan(&v); err == nil {
			clusterName = v.String
		}
	}

	// The cluster name becomes a label on every metric in this collector; if
	// it arrives with invalid bytes, sanitizing here covers every emission
	// point.
	clusterName = sanitizeLabel(clusterName)

	emit := func(desc *prometheus.Desc, variable string) {
		raw, ok := values[variable]
		if !ok {
			return
		}
		v, err := parseFloat(raw)
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "wsrep variable ignored", "variable", variable, "value", raw, "err", err)
			return
		}
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, clusterName)
	}

	emit(c.clusterSize, "wsrep_cluster_size")
	emit(c.flowControlPaused, "wsrep_flow_control_paused")
	emit(c.recvQueueAvg, "wsrep_local_recv_queue_avg")
	emit(c.sendQueueAvg, "wsrep_local_send_queue_avg")

	// wsrep_cluster_status is textual ("Primary"/"non-Primary").
	if raw, ok := values["wsrep_cluster_status"]; ok {
		value := 0.0
		if strings.EqualFold(strings.TrimSpace(raw), "primary") {
			value = 1
		}
		ch <- prometheus.MustNewConstMetric(c.clusterStatus, prometheus.GaugeValue, value, clusterName)
	}

	// wsrep_local_state is already numeric in the Galera protocol, but the
	// native numbering is 1=joining, 2=donor/desynced, 3=joined, 4=synced. The
	// specification calls for 0=joining, 1=donor, 2=joined, 3=synced, so 1 is
	// subtracted.
	if raw, ok := values["wsrep_local_state"]; ok {
		if v, err := parseFloat(raw); err == nil {
			ch <- prometheus.MustNewConstMetric(c.localState, prometheus.GaugeValue, v-1, clusterName)
		}
	}

	return nil
}
