package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// variableMapping links a SHOW GLOBAL VARIABLES variable to a metric.
type variableMapping struct {
	variable string
	metric   string
	help     string
}

// globalVariableMappings is the explicit list from section 2.2. All variables
// here are configuration values, so they are exposed as gauges.
var globalVariableMappings = []variableMapping{
	{"max_connections", "max_connections", "Value of max_connections."},
	{"innodb_buffer_pool_size", "innodb_buffer_pool_size_bytes", "InnoDB buffer pool size in bytes."},
	{"query_cache_size", "query_cache_size_bytes", "Query cache size in bytes."},
	{"thread_cache_size", "thread_cache_size", "Value of thread_cache_size."},
	{"wait_timeout", "wait_timeout_seconds", "Value of wait_timeout in seconds."},
	{"interactive_timeout", "interactive_timeout_seconds", "Value of interactive_timeout in seconds."},
	{"userstat", "userstat_enabled", "1 if the userstat variable is ON, 0 otherwise."},
}

// GlobalVariablesCollector collects an explicit subset of
// SHOW GLOBAL VARIABLES.
type GlobalVariablesCollector struct {
	base
	descs map[string]*prometheus.Desc
}

// NewGlobalVariablesCollector creates the global_variables collector.
func NewGlobalVariablesCollector(enabled bool, logger log.Logger, features FeatureProvider) *GlobalVariablesCollector {
	c := &GlobalVariablesCollector{
		base:  newBase("global_variables", "Selected subset of SHOW GLOBAL VARIABLES.", enabled, logger, features),
		descs: make(map[string]*prometheus.Desc, len(globalVariableMappings)),
	}

	for _, m := range globalVariableMappings {
		c.descs[strings.ToLower(m.variable)] = newDesc("", m.metric, m.help, nil)
	}

	return c
}

// Collect implements Collector.
func (c *GlobalVariablesCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL VARIABLES")
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

		// parseFloat converts ON/OFF to 1/0, which covers userstat_enabled.
		v, err := parseFloat(string(value))
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "global variable ignored", "variable", name, "err", err)
			continue
		}

		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v)
	}

	return rows.Err()
}
