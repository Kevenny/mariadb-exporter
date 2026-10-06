// Package config concentrates the parsing of command-line flags and
// environment variables of mariadb_exporter, as well as DSN normalization.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/prometheus/exporter-toolkit/web/kingpinflag"
)

// Web groups the HTTP server settings.
//
// The listen address, the TLS/authentication config file, and systemd socket
// activation live in ToolkitFlags, managed by Prometheus's exporter-toolkit —
// it is the one that implements TLS and basic auth based on
// --web.config.file.
type Web struct {
	TelemetryPath string
	MaxRequests   int

	// ToolkitFlags carries --web.listen-address (repeatable),
	// --web.config.file and --web.systemd-socket.
	ToolkitFlags *web.FlagConfig
}

// ListenAddresses returns the configured listen addresses, for logging.
func (w Web) ListenAddresses() []string {
	if w.ToolkitFlags == nil || w.ToolkitFlags.WebListenAddresses == nil {
		return nil
	}
	return *w.ToolkitFlags.WebListenAddresses
}

// WebConfigFile returns the path to the TLS/auth file, or an empty string.
func (w Web) WebConfigFile() string {
	if w.ToolkitFlags == nil || w.ToolkitFlags.WebConfigFile == nil {
		return ""
	}
	return *w.ToolkitFlags.WebConfigFile
}

// DataSource groups the MariaDB connection settings.
type DataSource struct {
	Name    string
	MaxOpen int
	MaxIdle int
	Timeout time.Duration
}

// Collectors groups the toggles and limits of each collector.
type Collectors struct {
	UserStat          bool
	TableStat         bool
	TableStatLimit    int
	IndexStat         bool
	IndexStatLimit    int
	ClientStat        bool
	QueryResponseTime bool
	MetadataLocks     bool
	Disks             bool
	Replication       bool
	Galera            bool
	InnoDB            bool
	GlobalStatus      bool
	GlobalVariables   bool
}

// Log groups the logging settings.
type Log struct {
	Level  string
	Format string
}

// PMM groups the metadata used to populate ConstLabels when the exporter is
// integrated with Percona PMM as an External Service (see
// mariadb_exporter_pmm_integration.md, section 3). These labels allow the
// cluster/environment/service filters to work in PMM dashboards.
type PMM struct {
	ServiceName    string
	Cluster        string
	Environment    string
	ReplicationSet string
}

// Config is the exporter's complete configuration.
type Config struct {
	Web           Web
	DataSource    DataSource
	Collectors    Collectors
	Log           Log
	PMM           PMM
	CustomMetrics []string
}

// defaultServiceName builds the default value for --pmm.service-name from the
// machine's hostname. If the hostname cannot be obtained, it falls back to a
// fixed value instead of leaving the flag without a default.
func defaultServiceName() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "mariadb"
	}
	return host + "-mariadb"
}

// ConstLabels builds the prometheus.Labels corresponding to the PMM
// integration metadata (mariadb_exporter_pmm_integration.md, section 3),
// ready for use in metric ConstLabels. Empty fields are omitted: not every
// installation runs behind a PMM, and an empty label would needlessly
// pollute the series.
//
// The return type is map[string]string instead of prometheus.Labels so as
// not to couple this config package to the Prometheus library —
// prometheus.Labels is already defined as this same type, so the value works
// directly wherever ConstLabels is expected.
func (p PMM) ConstLabels() map[string]string {
	labels := map[string]string{}
	if p.ServiceName != "" {
		labels["service_name"] = p.ServiceName
	}
	if p.Cluster != "" {
		labels["cluster"] = p.Cluster
	}
	if p.Environment != "" {
		labels["environment"] = p.Environment
	}
	if p.ReplicationSet != "" {
		labels["replication_set"] = p.ReplicationSet
	}
	return labels
}

// envDefault returns the value of the first defined environment variable
// among those given; if none is defined, it returns fallback. This lets
// flags have a default sourced from the environment, per section 6 of the
// specification.
func envDefault(fallback string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return fallback
}

// Register declares all flags on the given kingpin app and returns the
// Config that will be populated when app.Parse is called.
func Register(app *kingpin.Application) *Config {
	cfg := &Config{}

	// The toolkit registers --web.listen-address (repeatable),
	// --web.config.file and, on Linux, --web.systemd-socket. It is the one
	// that implements TLS and basic auth.
	cfg.Web.ToolkitFlags = kingpinflag.AddFlags(app, envDefault(":9104", "MARIADB_WEB_LISTEN_ADDRESS"))

	app.Flag("web.telemetry-path", "Path under which metrics are exposed.").
		Default(envDefault("/metrics", "MARIADB_WEB_TELEMETRY_PATH")).
		StringVar(&cfg.Web.TelemetryPath)
	app.Flag("web.max-requests", "Maximum number of concurrent scrapes (0 = unlimited).").
		Default(envDefault("0", "MARIADB_WEB_MAX_REQUESTS")).
		IntVar(&cfg.Web.MaxRequests)

	app.Flag("datasource.name", "DSN for connecting to MariaDB (default: env MARIADB_DSN).").
		Default(envDefault("", "MARIADB_DSN", "MARIADB_DATASOURCE_NAME")).
		StringVar(&cfg.DataSource.Name)
	app.Flag("datasource.max-open", "Maximum number of open connections in the pool.").
		Default(envDefault("3", "MARIADB_DATASOURCE_MAX_OPEN")).
		IntVar(&cfg.DataSource.MaxOpen)
	app.Flag("datasource.max-idle", "Maximum number of idle connections in the pool.").
		Default(envDefault("3", "MARIADB_DATASOURCE_MAX_IDLE")).
		IntVar(&cfg.DataSource.MaxIdle)
	app.Flag("datasource.timeout", "Query timeout in seconds.").
		Default(envDefault("30s", "MARIADB_DATASOURCE_TIMEOUT")).
		DurationVar(&cfg.DataSource.Timeout)

	app.Flag("collector.userstat", "Enables the userstat collector.").
		Default("true").BoolVar(&cfg.Collectors.UserStat)
	app.Flag("collector.tablestat", "Enables the tablestat collector.").
		Default("true").BoolVar(&cfg.Collectors.TableStat)
	app.Flag("collector.tablestat.limit", "Limit of tables per scrape.").
		Default("500").IntVar(&cfg.Collectors.TableStatLimit)
	app.Flag("collector.indexstat", "Enables the indexstat collector.").
		Default("true").BoolVar(&cfg.Collectors.IndexStat)
	app.Flag("collector.indexstat.limit", "Limit of indexes per scrape.").
		Default("1000").IntVar(&cfg.Collectors.IndexStatLimit)
	app.Flag("collector.clientstat", "Enables the clientstat collector.").
		Default("true").BoolVar(&cfg.Collectors.ClientStat)
	app.Flag("collector.query_response_time", "Enables the query_response_time collector.").
		Default("true").BoolVar(&cfg.Collectors.QueryResponseTime)
	app.Flag("collector.metadata_locks", "Enables the metadata_locks collector.").
		Default("true").BoolVar(&cfg.Collectors.MetadataLocks)
	app.Flag("collector.disks", "Enables the disks collector.").
		Default("true").BoolVar(&cfg.Collectors.Disks)
	app.Flag("collector.replication", "Enables the replication collector.").
		Default("true").BoolVar(&cfg.Collectors.Replication)
	app.Flag("collector.galera", "Enables the galera collector (opt-in).").
		Default("false").BoolVar(&cfg.Collectors.Galera)
	app.Flag("collector.innodb", "Enables the innodb collector.").
		Default("true").BoolVar(&cfg.Collectors.InnoDB)
	app.Flag("collector.global_status", "Enables the global_status collector.").
		Default("true").BoolVar(&cfg.Collectors.GlobalStatus)
	app.Flag("collector.global_variables", "Enables the global_variables collector.").
		Default("true").BoolVar(&cfg.Collectors.GlobalVariables)

	app.Flag("custom-metrics", "YAML file of custom metrics (repeatable).").
		PlaceHolder("FILE").StringsVar(&cfg.CustomMetrics)

	app.Flag("pmm.service-name", "Service name in the PMM inventory.").
		Default(envDefault(defaultServiceName(), "MARIADB_PMM_SERVICE_NAME")).
		StringVar(&cfg.PMM.ServiceName)
	app.Flag("pmm.cluster", "Cluster name for grouping in PMM.").
		Default(envDefault("", "MARIADB_PMM_CLUSTER")).
		StringVar(&cfg.PMM.Cluster)
	app.Flag("pmm.environment", "Environment for grouping in PMM (production, staging, dev).").
		Default(envDefault("production", "MARIADB_PMM_ENVIRONMENT")).
		StringVar(&cfg.PMM.Environment)
	app.Flag("pmm.replication-set", "Replication set name in PMM (optional).").
		Default(envDefault("", "MARIADB_PMM_REPLICATION_SET")).
		StringVar(&cfg.PMM.ReplicationSet)

	app.Flag("log.level", "Log level: debug, info, warn, error.").
		Default(envDefault("info", "MARIADB_LOG_LEVEL")).
		EnumVar(&cfg.Log.Level, "debug", "info", "warn", "error")
	app.Flag("log.format", "Log format: text, json.").
		Default(envDefault("text", "MARIADB_LOG_FORMAT")).
		EnumVar(&cfg.Log.Format, "text", "json")

	return cfg
}

// Validate checks whether the minimum configuration is present.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.DataSource.Name) == "" {
		return fmt.Errorf("DSN not provided: use --datasource.name or the MARIADB_DSN environment variable")
	}
	if _, err := NormalizeDSN(c.DataSource.Name); err != nil {
		return err
	}
	if c.Collectors.TableStatLimit < 0 {
		return fmt.Errorf("--collector.tablestat.limit cannot be negative")
	}
	if c.Collectors.IndexStatLimit < 0 {
		return fmt.Errorf("--collector.indexstat.limit cannot be negative")
	}
	// A --web.config.file that is missing, unreadable, or invalid is a
	// configuration failure: better to abort at startup than to come up
	// without the TLS/authentication the operator asked for, believing the
	// endpoint is protected. Content validation is done by the toolkit
	// itself, and it happens here — before opening the listener — so the
	// exporter never spends even a moment listening unprotected.
	if path := c.Web.WebConfigFile(); path != "" {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("--web.config.file %q inaccessible: %w", path, err)
		}
		if err := web.Validate(path); err != nil {
			return fmt.Errorf("--web.config.file %q invalid: %w", path, err)
		}
	}
	return nil
}

// NormalizeDSN converts the DSN provided by the user to the format accepted
// by the go-sql-driver/mysql driver.
//
// The specification uses the `mariadb://` prefix for semantic clarity, but
// the driver speaks the MySQL protocol and does not understand URL schemes.
// This function removes the prefix (`mariadb://` or `mysql://`) and returns
// the native DSN, for example:
//
//	mariadb://pmm:password@tcp(localhost:3306)/  ->  pmm:password@tcp(localhost:3306)/
//
// DSNs already in the driver's native format are returned unchanged.
func NormalizeDSN(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", fmt.Errorf("empty DSN")
	}

	_, dsn = splitScheme(dsn)

	// The driver requires the slash separating the address from the database
	// name. Without it, a DSN like "user:password@tcp(host:3306)" is
	// rejected in Open.
	if !strings.Contains(dsn, "/") {
		dsn += "/"
	}

	return dsn, nil
}

// splitScheme separates the optional mariadb:// or mysql:// prefix from the DSN.
func splitScheme(dsn string) (scheme, rest string) {
	for _, s := range []string{"mariadb://", "mysql://"} {
		if strings.HasPrefix(strings.ToLower(dsn), s) {
			return dsn[:len(s)], dsn[len(s):]
		}
	}
	return "", dsn
}

// RedactDSN removes the password from the DSN so it can appear in logs.
//
// It mirrors go-sql-driver/mysql's ParseDSN split rule exactly — the password
// runs from the first ':' to the last '@' before the last '/' — so any
// password the driver accepts (including ones containing ':' or '@', or a
// DSN with '@' in its query parameters) is masked in full.
func RedactDSN(dsn string) string {
	scheme, rest := splitScheme(dsn)

	end := strings.LastIndex(rest, "/")
	if end < 0 {
		end = len(rest)
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return dsn
	}
	colon := strings.Index(rest[:at], ":")
	if colon < 0 {
		return dsn
	}
	return scheme + rest[:colon] + ":***" + rest[at:]
}

// dsnQueryParams extracts the DSN's query parameters, if any. Used only in
// tests and diagnostics.
func dsnQueryParams(dsn string) (url.Values, error) {
	idx := strings.Index(dsn, "?")
	if idx < 0 {
		return url.Values{}, nil
	}
	return url.ParseQuery(dsn[idx+1:])
}
