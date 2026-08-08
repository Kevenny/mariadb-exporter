package config

import (
	"os"
	"path/filepath"
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

	require.Equal(t, []string{":9104"}, cfg.Web.ListenAddresses())
	require.Empty(t, cfg.Web.WebConfigFile(), "sem --web.config.file, o padrão é HTTP simples")
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

	// --pmm.service-name tem default derivado do hostname (nunca vazio) e
	// --pmm.environment tem default "production"; cluster e replication-set
	// ficam vazios até serem explicitamente configurados.
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

// O exporter-toolkit permite escutar em vários endereços, repetindo a flag.
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

// --web.config.file é o arquivo que habilita TLS e/ou basic auth.
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
	require.NoError(t, cfg.Validate(), "arquivo existente deve passar a validação")
}

// Um --web.config.file inexistente precisa falhar no startup: subir sem o TLS
// que o operador pediu daria uma falsa sensação de proteção.
func TestValidateRejectsMissingWebConfigFile(t *testing.T) {
	app := kingpin.New("teste", "")
	cfg := Register(app)

	_, err := app.Parse([]string{
		"--datasource.name=mariadb://u:p@tcp(h:3306)/",
		"--web.config.file=" + filepath.Join(t.TempDir(), "nao-existe.yml"),
	})
	require.NoError(t, err)

	err = cfg.Validate()
	require.Error(t, err, "arquivo de config web inexistente deveria abortar o startup")
	require.Contains(t, err.Error(), "web.config.file")
}

// O conteúdo do --web.config.file também é validado no startup — antes de abrir
// o listener. Sem isso, um hash bcrypt inválido só estouraria no primeiro
// request, com o exporter já escutando sem proteção nesse intervalo.
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
	require.Error(t, err, "hash bcrypt inválido deveria abortar o startup")
	require.Contains(t, err.Error(), "inválido")
}

// Um web-config.yml apontando para certificado inexistente deve falhar no
// startup, não ao aceitar a primeira conexão TLS.
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

	require.Error(t, cfg.Validate(), "certificado inexistente deveria abortar o startup")
}

// As flags --pmm.* alimentam config.PMM, usado para ConstLabels de integração
// com o PMM (mariadb_exporter_pmm_integration.md, seção 3).
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

// As flags --pmm.* também aceitam variáveis de ambiente, no mesmo padrão das
// demais flags do exporter.
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

// PMM.ConstLabels() só inclui os campos preenchidos, para não poluir séries em
// instalações que não usam PMM.
func TestPMMConstLabels(t *testing.T) {
	t.Run("todos os campos", func(t *testing.T) {
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

	t.Run("zero value nao gera labels", func(t *testing.T) {
		require.Empty(t, PMM{}.ConstLabels())
	})

	t.Run("campos parciais", func(t *testing.T) {
		pmm := PMM{ServiceName: "mariadb-host01"}
		require.Equal(t, map[string]string{"service_name": "mariadb-host01"}, pmm.ConstLabels())
	})
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
	require.Equal(t, []string{":9105"}, cfg.Web.ListenAddresses())
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
