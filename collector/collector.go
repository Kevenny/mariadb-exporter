// Package collector contains the Collector interface and the implementation of
// all MariaDB metric collectors.
package collector

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// Namespace is the prefix of every metric exposed by the exporter.
const Namespace = "mariadb"

// Collector is the interface every collector must implement.
type Collector interface {
	// Name returns the collector's name (used in flags and logs).
	Name() string

	// Help returns the collector's description for --help.
	Help() string

	// Enabled returns whether the collector is enabled (based on flags).
	Enabled() bool

	// Collect runs the queries and sends the metrics to the channel.
	Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error
}

// VersionInfo contains version information detected on the connection.
type VersionInfo struct {
	Major     int
	Minor     int
	Patch     int
	Full      string // full string, e.g.: "11.4.3-MariaDB"
	Comment   string // value of @@version_comment
	IsMariaDB bool
}

// AtLeast reports whether the detected version is greater than or equal to
// major.minor.patch.
func (v *VersionInfo) AtLeast(major, minor, patch int) bool {
	if v == nil {
		return false
	}
	if v.Major != major {
		return v.Major > major
	}
	if v.Minor != minor {
		return v.Minor > minor
	}
	return v.Patch >= patch
}

// String returns the version formatted as major.minor.patch.
func (v *VersionInfo) String() string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// FeatureFlags indicates which features are available on this instance.
type FeatureFlags struct {
	HasUserStat          bool // userstat plugin active
	HasQueryResponseTime bool // query_response_time plugin active
	HasMetadataLockInfo  bool // metadata_lock_info plugin active
	HasDisksPlugin       bool // disks plugin active
	HasGalera            bool // Galera/wsrep active
	IsReplica            bool // instance is a replica
}

// FeatureProvider delivers the most recent view of the detected features. The
// exporter refreshes this view periodically (section 9 of the specification),
// which is why collectors query through this interface instead of keeping a
// copy of the struct.
type FeatureProvider interface {
	Features() *FeatureFlags
	Version() *VersionInfo
}

// Availability is implemented by collectors that depend on a plugin or a
// server environment variable. The exporter uses the result to publish
// mariadb_collector_available{collector="name"}.
type Availability interface {
	// Available reports whether the collector's dependencies are satisfied.
	Available() bool
}

// warnOnce emits a warning only on the first occurrence, avoiding log spam on
// every scrape when a plugin is permanently absent (section 9, item 1).
type warnOnce struct {
	once   sync.Once
	logger log.Logger
}

func (w *warnOnce) warn(keyvals ...interface{}) {
	w.once.Do(func() {
		if w.logger == nil {
			return
		}
		_ = level.Warn(w.logger).Log(keyvals...)
	})
}

// base provides the common implementation of Name, Help and Enabled, plus the
// logger and access to the detected features. All collectors embed this
// struct.
type base struct {
	name     string
	help     string
	enabled  bool
	logger   log.Logger
	features FeatureProvider
	warned   warnOnce
}

func newBase(name, help string, enabled bool, logger log.Logger, features FeatureProvider) base {
	scoped := log.With(logger, "collector", name)
	return base{
		name:     name,
		help:     help,
		enabled:  enabled,
		logger:   scoped,
		features: features,
		warned:   warnOnce{logger: scoped},
	}
}

func (b *base) Name() string       { return b.name }
func (b *base) Help() string       { return b.help }
func (b *base) Enabled() bool      { return b.enabled }
func (b *base) Logger() log.Logger { return b.logger }

// featureFlags returns the current features, never nil, to simplify usage.
func (b *base) featureFlags() *FeatureFlags {
	if b.features == nil {
		return &FeatureFlags{}
	}
	if f := b.features.Features(); f != nil {
		return f
	}
	return &FeatureFlags{}
}

// newDesc is a shortcut for prometheus.NewDesc with the exporter's namespace
// already applied. If subsystem is empty, the name becomes namespace_name.
func newDesc(subsystem, name, help string, labels []string) *prometheus.Desc {
	return prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, subsystem, name),
		help,
		labels,
		nil,
	)
}

// utf8Replacement replaces each invalid byte in a label. The Unicode
// replacement character makes it evident in the dashboard that the source
// data is corrupted, instead of hiding it.
const utf8Replacement = "�"

// sanitizeLabel ensures the value can be used as a Prometheus label.
//
// client_golang panics when building a metric whose label is not valid UTF-8,
// and a panic inside Collect brings down the entire exporter process. This is
// perfectly reachable in production: usernames, schema, table or index names
// stored in latin1, or a binary blob in a column used as a label in custom
// metrics. Replacing the invalid bytes preserves the metric and keeps the
// exporter running.
func sanitizeLabel(value string) string {
	if utf8.ValidString(value) {
		return value
	}
	return strings.ToValidUTF8(value, utf8Replacement)
}

// sanitizeLabels applies sanitizeLabel to all given values, returning the same
// slice when nothing needs to be changed.
func sanitizeLabels(values []string) []string {
	needsFix := false
	for _, v := range values {
		if !utf8.ValidString(v) {
			needsFix = true
			break
		}
	}
	if !needsFix {
		return values
	}

	out := make([]string, len(values))
	for i, v := range values {
		out[i] = sanitizeLabel(v)
	}
	return out
}

// parseFloat converts to float64 the various formats the MySQL driver may
// return (nil, []byte, string, numbers). Non-numeric values return an error.
func parseFloat(value interface{}) (float64, error) {
	switch v := value.(type) {
	case nil:
		return 0, fmt.Errorf("null value")
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("non-finite value")
		}
		return v, nil
	case float32:
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("non-finite value")
		}
		return f, nil
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	case []byte:
		return parseFloatString(string(v))
	case string:
		return parseFloatString(v)
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("non-numeric type %T", value)
	}
}

// parseFloatString parses numeric strings as well as the textual boolean
// values that MariaDB uses in variables and in SHOW STATUS.
func parseFloatString(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty string")
	}
	switch strings.ToUpper(s) {
	case "ON", "YES", "TRUE", "ENABLED":
		return 1, nil
	case "OFF", "NO", "FALSE", "DISABLED", "NONE":
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("non-numeric value %q", s)
	}
	// ParseFloat accepts "NaN", "Inf" and "infinity". None of these is a useful
	// metric value: NaN disappears from graphs and makes alert comparisons fail
	// silently, and Inf distorts any aggregation. Better to treat it as an
	// invalid value and omit the metric.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("non-finite value %q", s)
	}
	return f, nil
}

// Registry keeps the list of collectors built and available to the exporter.
type Registry struct {
	collectors []Collector
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds collectors to the registry, ignoring nil values.
func (r *Registry) Register(collectors ...Collector) {
	for _, c := range collectors {
		if c != nil {
			r.collectors = append(r.collectors, c)
		}
	}
}

// All returns all registered collectors.
func (r *Registry) All() []Collector {
	out := make([]Collector, len(r.collectors))
	copy(out, r.collectors)
	return out
}

// Enabled returns only the collectors enabled via flag.
func (r *Registry) Enabled() []Collector {
	var out []Collector
	for _, c := range r.collectors {
		if c.Enabled() {
			out = append(out, c)
		}
	}
	return out
}

// Names returns the names of all registered collectors.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.collectors))
	for _, c := range r.collectors {
		out = append(out, c.Name())
	}
	return out
}
