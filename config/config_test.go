package config

import (
	"testing"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/stretchr/testify/require"
)

// O prefixo mariadb:// é semântico e precisa ser removido antes de chegar ao
// driver, que fala o protocolo MySQL (seção 7).
func TestNormalizeDSN(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "prefixo mariadb é removido",
			input: "mariadb://pmm:senha@tcp(localhost:3306)/",
			want:  "pmm:senha@tcp(localhost:3306)/",
		},
		{
			name:  "prefixo mysql é removido",
			input: "mysql://pmm:senha@tcp(localhost:3306)/",
			want:  "pmm:senha@tcp(localhost:3306)/",
		},
		{
			name:  "DSN nativo passa inalterado",
			input: "pmm:senha@tcp(localhost:3306)/",
			want:  "pmm:senha@tcp(localhost:3306)/",
		},
		{
			name:  "IP e porta",
			input: "mariadb://pmm:senha@tcp(192.168.1.10:3306)/",
			want:  "pmm:senha@tcp(192.168.1.10:3306)/",
		},
		{
			name:  "socket unix",
			input: "mariadb://pmm:senha@unix(/var/run/mysql/mysql.sock)/",
			want:  "pmm:senha@unix(/var/run/mysql/mysql.sock)/",
		},
		{
			name:  "com parâmetros de query",
			input: "mariadb://pmm:senha@tcp(localhost:3306)/?timeout=30s&readTimeout=30s",
			want:  "pmm:senha@tcp(localhost:3306)/?timeout=30s&readTimeout=30s",
		},
		{
			name:  "sem credenciais lê ~/.my.cnf",
			input: "mariadb://@tcp(localhost:3306)/?readTimeout=30s",
			want:  "@tcp(localhost:3306)/?readTimeout=30s",
		},
		{
			name:  "barra final é adicionada quando ausente",
			input: "mariadb://pmm:senha@tcp(localhost:3306)",
			want:  "pmm:senha@tcp(localhost:3306)/",
		},
		{
			name:  "prefixo em maiúsculas também é removido",
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

// A senha nunca deve aparecer nos logs.
func TestRedactDSN(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "senha é mascarada",
			input: "mariadb://pmm:senha_secreta@tcp(localhost:3306)/",
			want:  "mariadb://pmm:***@tcp(localhost:3306)/",
		},
		{
			name:  "sem senha permanece igual",
			input: "mariadb://@tcp(localhost:3306)/",
			want:  "mariadb://@tcp(localhost:3306)/",
		},
		{
			name:  "sem credenciais não mascara a porta",
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
	t.Run("DSN ausente é erro", func(t *testing.T) {
		cfg := &Config{}
		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "MARIADB_DSN")
	})

	t.Run("DSN válido passa", func(t *testing.T) {
		cfg := &Config{}
		cfg.DataSource.Name = "mariadb://pmm:senha@tcp(localhost:3306)/"
		require.NoError(t, cfg.Validate())
	})

	t.Run("limite negativo é erro", func(t *testing.T) {
		cfg := &Config{}
		cfg.DataSource.Name = "mariadb://pmm:senha@tcp(localhost:3306)/"
		cfg.Collectors.TableStatLimit = -1
		require.Error(t, cfg.Validate())
	})
}

// Os defaults precisam bater com a seção 6 da especificação.
func TestRegisterDefaults(t *testing.T) {
	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{"--datasource.name=mariadb://u:p@tcp(h:3306)/"})
	require.NoError(t, err)

	require.Equal(t, ":9104", cfg.Web.ListenAddress)
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

	// galera é opt-in.
	require.False(t, cfg.Collectors.Galera)

	require.Equal(t, "info", cfg.Log.Level)
	require.Equal(t, "text", cfg.Log.Format)
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

	require.Equal(t, ":9999", cfg.Web.ListenAddress)
	require.True(t, cfg.Collectors.Galera)
	require.False(t, cfg.Collectors.UserStat)
	require.Equal(t, 42, cfg.Collectors.TableStatLimit)
	require.Equal(t, "debug", cfg.Log.Level)
	require.Equal(t, "json", cfg.Log.Format)
	require.Equal(t, []string{"a.yml", "b.yml"}, cfg.CustomMetrics)
}

// As flags de web e datasource também aceitam variáveis de ambiente (seção 6).
func TestRegisterReadsEnv(t *testing.T) {
	t.Setenv("MARIADB_DSN", "mariadb://env:senha@tcp(envhost:3306)/")
	t.Setenv("MARIADB_WEB_LISTEN_ADDRESS", ":9105")
	t.Setenv("MARIADB_LOG_LEVEL", "warn")

	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse(nil)
	require.NoError(t, err)

	require.Equal(t, "mariadb://env:senha@tcp(envhost:3306)/", cfg.DataSource.Name)
	require.Equal(t, ":9105", cfg.Web.ListenAddress)
	require.Equal(t, "warn", cfg.Log.Level)
	require.NoError(t, cfg.Validate())
}

// A flag explícita tem precedência sobre a variável de ambiente.
func TestFlagOverridesEnv(t *testing.T) {
	t.Setenv("MARIADB_WEB_LISTEN_ADDRESS", ":9105")

	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--web.listen-address=:9200",
	})
	require.NoError(t, err)

	require.Equal(t, ":9200", cfg.Web.ListenAddress)
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
