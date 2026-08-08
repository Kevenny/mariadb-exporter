package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// statusMapping links a SHOW GLOBAL STATUS variable to an exposed metric.
type statusMapping struct {
	variable string // variable name in MariaDB
	metric   string // metric name, without the namespace
	help     string
	kind     prometheus.ValueType
}

// globalStatusMappings is the explicit list from section 2.2. Deliberately not
// a wildcard: exposing all of SHOW GLOBAL STATUS would generate high
// cardinality.
var globalStatusMappings = []statusMapping{
	{"Connections", "connections_total", "Total connection attempts to the server.", prometheus.CounterValue},
	{"Max_used_connections", "max_used_connections", "Maximum number of simultaneous connections ever recorded.", prometheus.GaugeValue},
	{"Aborted_connects", "aborted_connects_total", "Total connection attempts to the server that failed.", prometheus.CounterValue},
	{"Aborted_clients", "aborted_clients_total", "Total connections aborted by clients that terminated without closing the connection.", prometheus.CounterValue},
	{"Bytes_received", "bytes_received_total", "Total bytes received from all clients.", prometheus.CounterValue},
	{"Bytes_sent", "bytes_sent_total", "Total bytes sent to all clients.", prometheus.CounterValue},
	{"Questions", "questions_total", "Total statements executed by the server.", prometheus.CounterValue},
	{"Slow_queries", "slow_queries_total", "Total queries that exceeded long_query_time.", prometheus.CounterValue},
	{"Open_files", "open_files", "Number of files currently open.", prometheus.GaugeValue},
	{"Open_tables", "open_tables", "Number of tables currently open.", prometheus.GaugeValue},
	{"Table_open_cache_hits", "table_open_cache_hits_total", "Total hits in the open tables cache.", prometheus.CounterValue},
	{"Table_open_cache_misses", "table_open_cache_misses_total", "Total misses in the open tables cache.", prometheus.CounterValue},
	{"Created_tmp_tables", "created_tmp_tables_total", "Total temporary tables created in memory.", prometheus.CounterValue},
	{"Created_tmp_disk_tables", "created_tmp_disk_tables_total", "Total temporary tables created on disk.", prometheus.CounterValue},
	{"Select_full_join", "select_full_join_total", "Total joins without an index that required a full scan.", prometheus.CounterValue},
	{"Select_scan", "select_scan_total", "Total SELECTs that did a full table scan on the first table.", prometheus.CounterValue},
	{"Sort_merge_passes", "sort_merge_passes_total", "Total merge sort passes executed.", prometheus.CounterValue},
	{"Uptime", "uptime_seconds", "Time in seconds since the server started.", prometheus.GaugeValue},
}

// GlobalStatusCollector collects an explicit subset of SHOW GLOBAL STATUS.
type GlobalStatusCollector struct {
	base
	descs map[string]*prometheus.Desc
	kinds map[string]prometheus.ValueType
}

// NewGlobalStatusCollector creates the global_status collector.
func NewGlobalStatusCollector(enabled bool, logger log.Logger, features FeatureProvider) *GlobalStatusCollector {
	c := &GlobalStatusCollector{
		base:  newBase("global_status", "Selected subset of SHOW GLOBAL STATUS.", enabled, logger, features),
		descs: make(map[string]*prometheus.Desc, len(globalStatusMappings)),
		kinds: make(map[string]prometheus.ValueType, len(globalStatusMappings)),
	}

	for _, m := range globalStatusMappings {
		// The key is lowercase because MariaDB is not consistent about
		// variable capitalization across versions.
		key := strings.ToLower(m.variable)
		c.descs[key] = newDesc("", m.metric, m.help, nil)
		c.kinds[key] = m.kind
	}

	return c
}

// Collect implements Collector.
func (c *GlobalStatusCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL STATUS")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var value sql.RawBytes

		if err := rows.Scan(&name, &value); err != nil {
			return err
		}

		key := strings.ToLower(name)
		desc, ok := c.descs[key]
		if !ok {
			continue
		}

		v, err := parseFloat(string(value))
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "status variable ignored", "variable", name, "err", err)
			continue
		}

		ch <- prometheus.MustNewConstMetric(desc, c.kinds[key], v)
	}

	return rows.Err()
}
