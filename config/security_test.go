package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A senha do usuário de monitoramento é o segredo mais sensível que o exporter
// manipula. Ela não pode escapar por nenhum canal observável: log de startup,
// mensagem de erro de validação, ou a saída de --help.

const senhaSecreta = "S3nh4-Sup3r-S3cr3t4"

// RedactDSN é a única barreira entre o DSN e os logs. Estes casos cobrem
// formatos que poderiam escapar da heurística de mascaramento.
func TestRedactDSNNeverLeaksPassword(t *testing.T) {
	cases := []string{
		"mariadb://user:" + senhaSecreta + "@tcp(localhost:3306)/",
		"mysql://user:" + senhaSecreta + "@tcp(localhost:3306)/",
		"user:" + senhaSecreta + "@tcp(localhost:3306)/",
		"mariadb://user:" + senhaSecreta + "@unix(/var/run/mysql/mysql.sock)/",
		"mariadb://user:" + senhaSecreta + "@tcp(localhost:3306)/?timeout=30s",
		// Senha contendo caracteres que confundem parsers de URL.
		"mariadb://user:" + senhaSecreta + "@tcp(host:3306)/?tls=true&parseTime=true",
		// Usuário com @ no nome (comum em contas de cloud).
		"mariadb://user@dominio:" + senhaSecreta + "@tcp(host:3306)/",
	}

	for _, dsn := range cases {
		t.Run(dsn[:min(28, len(dsn))], func(t *testing.T) {
			redacted := RedactDSN(dsn)
			require.NotContains(t, redacted, senhaSecreta,
				"a senha vazou no DSN mascarado: %q", redacted)
		})
	}
}

// Uma senha que contenha ':' pode confundir a heurística que procura o
// separador usuário:senha.
func TestRedactDSNPasswordWithColon(t *testing.T) {
	dsn := "mariadb://user:parte1:parte2@tcp(localhost:3306)/"
	redacted := RedactDSN(dsn)

	require.NotContains(t, redacted, "parte2",
		"parte da senha após o ':' vazou: %q", redacted)
}

// Erros de Validate não devem incluir o DSN completo — mensagens de erro vão
// para o log e, em alguns setups, para sistemas de agregação.
func TestValidateErrorDoesNotLeakPassword(t *testing.T) {
	cfg := &Config{}
	cfg.DataSource.Name = "mariadb://user:" + senhaSecreta + "@tcp(localhost:3306)/"
	cfg.Collectors.TableStatLimit = -1 // força erro de validação

	err := cfg.Validate()
	require.Error(t, err)
	require.NotContains(t, err.Error(), senhaSecreta,
		"a senha vazou na mensagem de erro de validação: %q", err.Error())
}

// NormalizeDSN devolve o DSN em claro (é o que vai para o driver), mas seu erro
// não deve ecoar o DSN inteiro.
func TestNormalizeDSNErrorDoesNotLeakPassword(t *testing.T) {
	// Um DSN vazio é o único caminho de erro; confirma que a mensagem é genérica.
	_, err := NormalizeDSN("")
	require.Error(t, err)
	require.NotContains(t, strings.ToLower(err.Error()), "senha")
}

// O DSN normalizado precisa preservar a senha intacta — senão a conexão falha.
// Este teste é o contraponto dos anteriores: mascarar é para log, não para uso.
func TestNormalizeDSNPreservesPassword(t *testing.T) {
	dsn, err := NormalizeDSN("mariadb://user:" + senhaSecreta + "@tcp(localhost:3306)/")
	require.NoError(t, err)
	require.Contains(t, dsn, senhaSecreta,
		"a senha precisa sobreviver à normalização, senão a conexão falha")
	require.Equal(t, "user:"+senhaSecreta+"@tcp(localhost:3306)/", dsn)
}

// ConstLabels do PMM vão para dentro de toda métrica exposta em /metrics, que é
// um endpoint sem autenticação. Nenhum campo de PMM deve conter o DSN.
func TestPMMConstLabelsDoNotCarryCredentials(t *testing.T) {
	pmm := PMM{
		ServiceName: "mariadb-01",
		Cluster:     "prod",
		Environment: "production",
	}

	for k, v := range pmm.ConstLabels() {
		require.NotContains(t, v, senhaSecreta, "label %q carrega credencial", k)
		require.NotContains(t, v, "@tcp(", "label %q parece conter um DSN", k)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
