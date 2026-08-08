package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/stretchr/testify/require"
)

// The mariadb:// prefix is semantic and needs to be removed before reaching
// the driver, which speaks the MySQL protocol (section 7).
func TestNormalizeDSN(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "mariadb prefix is removed",
			input: "mariadb://pmm:senha@tcp(localhost:3306)/",
			want:  "pmm:senha@tcp(localhost:3306)/",
		},
		{
			name:  "mysql prefix is removed",
			input: "mysql://pmm:senha@tcp(localhost:3306)/",
			want:  "pmm:senha@tcp(localhost:3306)/",
		},
		{
			name:  "native DSN passes through unchanged",
			input: "pmm:senha@tcp(localhost:3306)/",
			want:  "pmm:senha@tcp(localhost:3306)/",
		},
		{
			name:  "IP and port",
			input: "mariadb://pmm:senha@tcp(192.168.1.10:3306)/",
			want:  "pmm:senha@tcp(192.168.1.10:3306)/",
		},
		{
			name:  "unix socket",
			input: "mariadb://pmm:senha@unix(/var/run/mysql/mysql.sock)/",
			want:  "pmm:senha@unix(/var/run/mysql/mysql.sock)/",
		},
		{
			name:  "with query parameters",
			input: "mariadb://pmm:senha@tcp(localhost:3306)/?timeout=30s&readTimeout=30s",
			want:  "pmm:senha@tcp(localhost:3306)/?timeout=30s&readTimeout=30s",
		},
		{
			name:  "without credentials reads ~/.my.cnf",
			input: "mariadb://@tcp(localhost:3306)/?readTimeout=30s",
			want:  "@tcp(localhost:3306)/?readTimeout=30s",
		},
		{
			name:  "trailing slash is added when missing",
			input: "mariadb://pmm:senha@tcp(localhost:3306)",
			want:  "pmm:senha@tcp(localhost:3306)/",
		},
		{
			name:  "uppercase prefix is also removed",
			input: "MariaDB://pmm:senha@tcp(localhost:3306)/",
			want:  "pmm:senha@tcp(localhost:3306)/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDSN(tc.input)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestNormalizeDSNEmpty(t *testing.T) {
	_, err := NormalizeDSN("   ")
	require.Error(t, err)
}

// The password must never appear in logs.
func TestRedactDSN(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "password is masked",
			input: "mariadb://pmm:senha_secreta@tcp(localhost:3306)/",
			want:  "mariadb://pmm:***@tcp(localhost:3306)/",
		},
		{
			name:  "without password stays the same",
			input: "mariadb://@tcp(localhost:3306)/",
			want:  "mariadb://@tcp(localhost:3306)/",
		},
		{
			name:  "without credentials does not mask the port",
			input: "tcp(localhost:3306)/",
			want:  "tcp(localhost:3306)/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, RedactDSN(tc.input))
			require.NotContains(t, RedactDSN(tc.input), "senha_secreta")
		})
	}
}

func TestValidate(t *testing.T) {
	t.Run("missing DSN is an error", func(t *testing.T) {
		cfg := &Config{}
		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "MARIADB_DSN")
	})

	t.Run("valid DSN passes", func(t *testing.T) {
		cfg := &Config{}
		cfg.DataSource.Name = "mariadb://pmm:senha@tcp(localhost:3306)/"
		require.NoError(t, cfg.Validate())
	})

	t.Run("negative limit is an error", func(t *testing.T) {
		cfg := &Config{}
		cfg.DataSource.Name = "mariadb://pmm:senha@tcp(localhost:3306)/"
		cfg.Collectors.TableStatLimit = -1
		require.Error(t, cfg.Validate())
	})
}

// The defaults need to match section 6 of the specification.
func TestRegisterDefaults(t *testing.T) {
	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{"--datasource.name=mariadb://u:p@tcp(h:3306)/"})
	require.NoError(t, err)

	require.Equal(t, []string{":9104"}, cfg.Web.ListenAddresses())
	require.Empty(t, cfg.Web.WebConfigFile(), "without --web.config.file, the default is plain HTTP")
	require.Equal(t, "/metrics", cfg.Web.TelemetryPath)
	require.Equal(t, 0, cfg.Web.MaxRequests)

	require.Equal(t, 3, cfg.DataSource.MaxOpen)
	require.Equal(t, 3, cfg.DataSource.MaxIdle)
	require.Equal(t, 30*time.Second, cfg.DataSource.Timeout)

	require.True(t, cfg.Collectors.UserStat)
	require.True(t, cfg.Collectors.TableStat)
	require.Equal(t, 500, cfg.Collectors.TableStatLimit)
	require.True(t, cfg.Collectors.IndexStat)
	require.Equal(t, 1000, cfg.Collectors.IndexStatLimit)
	require.True(t, cfg.Collectors.ClientStat)
	require.True(t, cfg.Collectors.QueryResponseTime)
	require.True(t, cfg.Collectors.MetadataLocks)
	require.True(t, cfg.Collectors.Disks)
	require.True(t, cfg.Collectors.Replication)
	require.True(t, cfg.Collectors.InnoDB)
	require.True(t, cfg.Collectors.GlobalStatus)
	require.True(t, cfg.Collectors.GlobalVariables)

	// galera is opt-in.
	require.False(t, cfg.Collectors.Galera)

	require.Equal(t, "info", cfg.Log.Level)
	require.Equal(t, "text", cfg.Log.Format)

	// --pmm.service-name has a default derived from the hostname (never
	// empty), and --pmm.environment has the default "production"; cluster
	// and replication-set stay empty until explicitly configured.
	require.NotEmpty(t, cfg.PMM.ServiceName)
	require.Equal(t, "production", cfg.PMM.Environment)
	require.Empty(t, cfg.PMM.Cluster)
	require.Empty(t, cfg.PMM.ReplicationSet)
}

func TestRegisterFlagOverrides(t *testing.T) {
	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--web.listen-address=:9999",
		"--collector.galera",
		"--no-collector.userstat",
		"--collector.tablestat.limit=42",
		"--log.level=debug",
		"--log.format=json",
		"--custom-metrics=a.yml",
		"--custom-metrics=b.yml",
	})
	require.NoError(t, err)

	require.Equal(t, []string{":9999"}, cfg.Web.ListenAddresses())
	require.True(t, cfg.Collectors.Galera)
	require.False(t, cfg.Collectors.UserStat)
	require.Equal(t, 42, cfg.Collectors.TableStatLimit)
	require.Equal(t, "debug", cfg.Log.Level)
	require.Equal(t, "json", cfg.Log.Format)
	require.Equal(t, []string{"a.yml", "b.yml"}, cfg.CustomMetrics)
}

// The exporter-toolkit allows listening on multiple addresses by repeating the flag.
func TestRegisterMultipleListenAddresses(t *testing.T) {
	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--web.listen-address=127.0.0.1:9104",
		"--web.listen-address=[::1]:9104",
	})
	require.NoError(t, err)

	require.Equal(t, []string{"127.0.0.1:9104", "[::1]:9104"}, cfg.Web.ListenAddresses())
}

// --web.config.file is the file that enables TLS and/or basic auth.
func TestRegisterWebConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-config.yml")
	require.NoError(t, os.WriteFile(path, []byte("basic_auth_users: {}\n"), 0o600))

	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--web.config.file=" + path,
	})
	require.NoError(t, err)

	require.Equal(t, path, cfg.Web.WebConfigFile())
	require.NoError(t, cfg.Validate(), "existing file should pass validation")
}

// A missing --web.config.file must fail at startup: coming up without the
// TLS the operator asked for would give a false sense of protection.
func TestValidateRejectsMissingWebConfigFile(t *testing.T) {
	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--web.config.file=" + filepath.Join(t.TempDir(), "nao-existe.yml"),
	})
	require.NoError(t, err)

	err = cfg.Validate()
	require.Error(t, err, "a missing web config file should abort startup")
	require.Contains(t, err.Error(), "web.config.file")
}

// The content of --web.config.file is also validated at startup — before
// opening the listener. Without this, an invalid bcrypt hash would only blow
// up on the first request, with the exporter already listening unprotected
// during that window.
func TestValidateRejectsMalformedWebConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-config.yml")
	require.NoError(t, os.WriteFile(path,
		[]byte("basic_auth_users:\n  prometheus: nao-e-um-hash-bcrypt\n"), 0o600))

	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--web.config.file=" + path,
	})
	require.NoError(t, err)

	err = cfg.Validate()
	require.Error(t, err, "invalid bcrypt hash should abort startup")
	require.Contains(t, err.Error(), "invalid")
}

// A web-config.yml pointing to a missing certificate should fail at
// startup, not when accepting the first TLS connection.
func TestValidateRejectsWebConfigWithMissingCert(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "web-config.yml")
	require.NoError(t, os.WriteFile(path, []byte(
		"tls_server_config:\n"+
			"  cert_file: "+filepath.Join(dir, "nao-existe.crt")+"\n"+
			"  key_file: "+filepath.Join(dir, "nao-existe.key")+"\n",
	), 0o600))

	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--web.config.file=" + path,
	})
	require.NoError(t, err)

	require.Error(t, cfg.Validate(), "missing certificate should abort startup")
}

// The --pmm.* flags feed config.PMM, used for PMM integration ConstLabels
// (mariadb_exporter_pmm_integration.md, section 3).
func TestRegisterPMMFlags(t *testing.T) {
	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--pmm.service-name=mariadb-host01",
		"--pmm.cluster=prod-cluster",
		"--pmm.environment=staging",
		"--pmm.replication-set=mariadb-11-4-primary",
	})
	require.NoError(t, err)

	require.Equal(t, "mariadb-host01", cfg.PMM.ServiceName)
	require.Equal(t, "prod-cluster", cfg.PMM.Cluster)
	require.Equal(t, "staging", cfg.PMM.Environment)
	require.Equal(t, "mariadb-11-4-primary", cfg.PMM.ReplicationSet)
}

// The --pmm.* flags also accept environment variables, following the same
// pattern as the exporter's other flags.
func TestRegisterPMMFlagsFromEnv(t *testing.T) {
	t.Setenv("MARIADB_PMM_SERVICE_NAME", "mariadb-env-host")
	t.Setenv("MARIADB_PMM_CLUSTER", "env-cluster")
	t.Setenv("MARIADB_PMM_ENVIRONMENT", "dev")
	t.Setenv("MARIADB_PMM_REPLICATION_SET", "env-repl-set")

	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{"--datasource.name=mariadb://u:p@tcp(h:3306)/"})
	require.NoError(t, err)

	require.Equal(t, "mariadb-env-host", cfg.PMM.ServiceName)
	require.Equal(t, "env-cluster", cfg.PMM.Cluster)
	require.Equal(t, "dev", cfg.PMM.Environment)
	require.Equal(t, "env-repl-set", cfg.PMM.ReplicationSet)
}

// PMM.ConstLabels() only includes the populated fields, so as not to pollute
// series in installations that don't use PMM.
func TestPMMConstLabels(t *testing.T) {
	t.Run("all fields", func(t *testing.T) {
		pmm := PMM{
			ServiceName:    "mariadb-host01",
			Cluster:        "prod-cluster",
			Environment:    "production",
			ReplicationSet: "mariadb-11-4-primary",
		}
		require.Equal(t, map[string]string{
			"service_name":    "mariadb-host01",
			"cluster":         "prod-cluster",
			"environment":     "production",
			"replication_set": "mariadb-11-4-primary",
		}, pmm.ConstLabels())
	})

	t.Run("zero value generates no labels", func(t *testing.T) {
		require.Empty(t, PMM{}.ConstLabels())
	})

	t.Run("partial fields", func(t *testing.T) {
		pmm := PMM{ServiceName: "mariadb-host01"}
		require.Equal(t, map[string]string{"service_name": "mariadb-host01"}, pmm.ConstLabels())
	})
}

// The web and datasource flags also accept environment variables (section 6).
func TestRegisterReadsEnv(t *testing.T) {
	t.Setenv("MARIADB_DSN", "mariadb://env:senha@tcp(envhost:3306)/")
	t.Setenv("MARIADB_WEB_LISTEN_ADDRESS", ":9105")
	t.Setenv("MARIADB_LOG_LEVEL", "warn")

	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse(nil)
	require.NoError(t, err)

	require.Equal(t, "mariadb://env:senha@tcp(envhost:3306)/", cfg.DataSource.Name)
	require.Equal(t, []string{":9105"}, cfg.Web.ListenAddresses())
	require.Equal(t, "warn", cfg.Log.Level)
	require.NoError(t, cfg.Validate())
}

// The explicit flag takes precedence over the environment variable.
func TestFlagOverridesEnv(t *testing.T) {
	t.Setenv("MARIADB_WEB_LISTEN_ADDRESS", ":9105")

	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--web.listen-address=:9200",
	})
	require.NoError(t, err)

	require.Equal(t, []string{":9200"}, cfg.Web.ListenAddresses())
}

func TestDSNQueryParams(t *testing.T) {
	params, err := dsnQueryParams("u:p@tcp(h:3306)/?timeout=30s&readTimeout=15s")
	require.NoError(t, err)
	require.Equal(t, "30s", params.Get("timeout"))
	require.Equal(t, "15s", params.Get("readTimeout"))

	empty, err := dsnQueryParams("u:p@tcp(h:3306)/")
	require.NoError(t, err)
	require.Empty(t, empty)
}
