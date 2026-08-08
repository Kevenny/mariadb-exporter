package collector

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"
)

// Usage is the usage type of a column in the custom metrics YAML.
const (
	usageLabel   = "LABEL"
	usageCounter = "COUNTER"
	usageGauge   = "GAUGE"
)

// columnSpec describes how a column of the result set should be handled.
type columnSpec struct {
	Usage       string `yaml:"usage"`
	Description string `yaml:"description"`
}

// metricSpec is the definition of a custom metric in the YAML.
//
// The format follows the specification (section 10): the map key is the
// metric name, `query` is the SQL and `metrics` is a list of column -> spec
// maps.
type metricSpec struct {
	Query   string                  `yaml:"query"`
	Metrics []map[string]columnSpec `yaml:"metrics"`
}

// customMetric is the compiled form of a metricSpec, ready for scraping.
type customMetric struct {
	name       string
	query      string
	labelCols  []string
	valueCols  []string
	descs      map[string]*prometheus.Desc
	valueKinds map[string]prometheus.ValueType
}

// CustomMetricsCollector runs user-defined queries from YAML files.
type CustomMetricsCollector struct {
	base
	metrics []customMetric
}

// NewCustomMetricsCollector loads the given YAML files and returns the
// collector. An invalid file is a configuration error and aborts startup.
//
// constLabels are the PMM integration metadata (service_name, cluster,
// environment, replication_set). Applying them to custom metrics as well is
// what allows filtering them in dashboards alongside the native metrics —
// without this, a panel filtered by cluster simply wouldn't find the series.
func NewCustomMetricsCollector(paths []string, logger log.Logger, features FeatureProvider, constLabels prometheus.Labels) (*CustomMetricsCollector, error) {
	c := &CustomMetricsCollector{
		base: newBase("custom_metrics", "User-defined metrics from YAML files.", len(paths) > 0, logger, features),
	}

	for _, path := range paths {
		metrics, err := loadCustomMetricsFile(path, constLabels)
		if err != nil {
			return nil, fmt.Errorf("custom metrics %q: %w", path, err)
		}
		c.metrics = append(c.metrics, metrics...)
	}

	return c, nil
}

// loadCustomMetricsFile reads and compiles a custom metrics YAML file.
func loadCustomMetricsFile(path string, constLabels prometheus.Labels) ([]customMetric, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var raw map[string]metricSpec
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}

	// The names are sorted so that registration order is deterministic across
	// runs, since map iteration in Go is random.
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]customMetric, 0, len(names))
	for _, name := range names {
		compiled, err := compileCustomMetric(name, raw[name], constLabels)
		if err != nil {
			return nil, err
		}
		out = append(out, *compiled)
	}

	return out, nil
}

// metricNameRe and labelNameRe are Prometheus's name grammars. A name outside
// them makes client_golang panic when building the metric, which would bring
// down the exporter at runtime — that's why validation happens at load time,
// where the error is just a startup configuration failure.
var (
	metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// compileCustomMetric validates the spec and precomputes the Descs.
func compileCustomMetric(name string, spec metricSpec, constLabels prometheus.Labels) (*customMetric, error) {
	if strings.TrimSpace(spec.Query) == "" {
		return nil, fmt.Errorf("metric %q: query field is required", name)
	}
	if len(spec.Metrics) == 0 {
		return nil, fmt.Errorf("metric %q: metrics field is required", name)
	}
	if !metricNameRe.MatchString(name) {
		return nil, fmt.Errorf(
			"metric %q: invalid name for Prometheus (use only letters, digits, _ and :, starting with a letter, _ or :)",
			name,
		)
	}

	cm := &customMetric{
		name:       name,
		query:      spec.Query,
		descs:      make(map[string]*prometheus.Desc),
		valueKinds: make(map[string]prometheus.ValueType),
	}

	// First pass: separates label columns from value columns, since the Descs
	// of the value columns need to know all labels.
	type valueCol struct {
		column      string
		description string
		kind        prometheus.ValueType
	}
	var valueCols []valueCol

	// A column can only have one role: repeated as LABEL would generate
	// duplicate labels in the Desc (panic in client_golang), and declared as
	// LABEL and value at the same time would be ambiguous.
	seen := make(map[string]string)

	for _, entry := range spec.Metrics {
		for column, colSpec := range entry {
			usage := strings.ToUpper(strings.TrimSpace(colSpec.Usage))

			if previous, dup := seen[column]; dup {
				return nil, fmt.Errorf(
					"metric %q: column %q declared more than once (as %s and %s)",
					name, column, previous, usage,
				)
			}
			seen[column] = usage

			switch usage {
			case usageLabel:
				if !labelNameRe.MatchString(column) {
					return nil, fmt.Errorf(
						"metric %q: %q is not a valid Prometheus label name (use only letters, digits and _, starting with a letter or _)",
						name, column,
					)
				}
				cm.labelCols = append(cm.labelCols, column)
			case usageCounter:
				valueCols = append(valueCols, valueCol{column, colSpec.Description, prometheus.CounterValue})
			case usageGauge:
				valueCols = append(valueCols, valueCol{column, colSpec.Description, prometheus.GaugeValue})
			default:
				return nil, fmt.Errorf(
					"metric %q, column %q: invalid usage %q (use LABEL, COUNTER or GAUGE)",
					name, column, colSpec.Usage,
				)
			}
		}
	}

	if len(valueCols) == 0 {
		return nil, fmt.Errorf("metric %q: at least one COUNTER or GAUGE column is required", name)
	}

	for _, vc := range valueCols {
		help := vc.description
		if help == "" {
			help = fmt.Sprintf("Custom metric %s (column %s).", name, vc.column)
		}

		// With a single value column, the metric name is the one declared in
		// the YAML; with several, the column name is suffixed to avoid
		// collisions.
		metricName := name
		if len(valueCols) > 1 {
			metricName = name + "_" + vc.column
			// The suffix goes into the final name, so it needs to stay valid.
			if !metricNameRe.MatchString(metricName) {
				return nil, fmt.Errorf(
					"metric %q: column %q generates the invalid Prometheus name %q",
					name, vc.column, metricName,
				)
			}
		}

		cm.valueCols = append(cm.valueCols, vc.column)
		cm.descs[vc.column] = prometheus.NewDesc(metricName, help, cm.labelCols, constLabels)
		cm.valueKinds[vc.column] = vc.kind
	}

	return cm, nil
}

// Collect implements Collector. An error in one query does not prevent the
// others from running; the first error is returned at the end.
func (c *CustomMetricsCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	var firstErr error

	for _, cm := range c.metrics {
		if err := c.collectOne(ctx, db, ch, cm); err != nil {
			_ = level.Error(c.Logger()).Log("msg", "custom metric failed", "metric", cm.name, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("custom metric %q: %w", cm.name, err)
			}
		}
	}

	return firstErr
}

func (c *CustomMetricsCollector) collectOne(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric, cm customMetric) error {
	rows, err := db.QueryContext(ctx, cm.query)
	if err != nil {
		return err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return err
	}

	// Column index by name (case-insensitive) to locate labels and values
	// regardless of the order in the SELECT.
	index := make(map[string]int, len(columns))
	for i, col := range columns {
		index[strings.ToLower(col)] = i
	}

	for _, want := range append(append([]string{}, cm.labelCols...), cm.valueCols...) {
		if _, ok := index[strings.ToLower(want)]; !ok {
			return fmt.Errorf("column %q declared in the YAML does not exist in the query's result set", want)
		}
	}

	for rows.Next() {
		values := make([]sql.RawBytes, len(columns))
		scanArgs := make([]interface{}, len(columns))
		for i := range values {
			scanArgs[i] = &values[i]
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return err
		}

		// Label columns can contain anything — including binary blobs,
		// depending on the operator's query. Sanitizing avoids client_golang
		// panicking on labels that are not valid UTF-8.
		labelValues := make([]string, 0, len(cm.labelCols))
		for _, col := range cm.labelCols {
			labelValues = append(labelValues, sanitizeLabel(string(values[index[strings.ToLower(col)]])))
		}

		for _, col := range cm.valueCols {
			raw := string(values[index[strings.ToLower(col)]])

			v, err := parseFloat(raw)
			if err != nil {
				_ = level.Debug(c.Logger()).Log(
					"msg", "custom metric value ignored",
					"metric", cm.name, "column", col, "value", raw, "err", err,
				)
				continue
			}

			ch <- prometheus.MustNewConstMetric(
				cm.descs[col], cm.valueKinds[col], v, labelValues...,
			)
		}
	}

	return rows.Err()
}
