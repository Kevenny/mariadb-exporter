package config

import (
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// The monitoring user's password is the most sensitive secret the exporter
// handles. It must not escape through any observable channel: startup log,
// validation error message, or --help output.

const senhaSecreta = "S3nh4-Sup3r-S3cr3t4"

func redact(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := DataSource{Name: dsn}.DriverConfig()
	require.NoError(t, err)
	return Redacted(cfg)
}

// Redacted is the only barrier between the connection settings and the logs.
// These cases cover formats that could escape a string-based masking.
func TestRedactedNeverLeaksPassword(t *testing.T) {
	cases := []string{
		"mariadb://user:" + senhaSecreta + "@tcp(localhost:3306)/",
		"mysql://user:" + senhaSecreta + "@tcp(localhost:3306)/",
		"user:" + senhaSecreta + "@tcp(localhost:3306)/",
		"mariadb://user:" + senhaSecreta + "@unix(/var/run/mysql/mysql.sock)/",
		"mariadb://user:" + senhaSecreta + "@tcp(localhost:3306)/?timeout=30s",
		// Password containing characters that confuse URL parsers.
		"mariadb://user:" + senhaSecreta + "@tcp(host:3306)/?tls=true&parseTime=true",
		// User with @ in the name (common in cloud accounts).
		"mariadb://user@dominio:" + senhaSecreta + "@tcp(host:3306)/",
	}

	for _, dsn := range cases {
		t.Run(dsn[:min(28, len(dsn))], func(t *testing.T) {
			redacted := redact(t, dsn)
			require.NotContains(t, redacted, senhaSecreta,
				"the password leaked in the masked DSN: %q", redacted)
		})
	}
}

// A password containing ':' must be masked in full, not only after its last ':'.
func TestRedactedPasswordWithColon(t *testing.T) {
	redacted := redact(t, "mariadb://user:parte1:parte2@tcp(localhost:3306)/")
	require.Equal(t, "user:***@tcp(localhost:3306)/", redacted)
}

// The masking must agree with how the driver itself splits the DSN: whatever
// the driver would use as the password must never reach the log.
func TestRedactedMatchesDriverParsing(t *testing.T) {
	dsns := []string{
		"mariadb://user:" + senhaSecreta + "@tcp(localhost:3306)/",
		"user:pa:ss:" + senhaSecreta + "@tcp(h:3306)/db",
		"user:p@ss@" + senhaSecreta + "@tcp(h:3306)/db?timeout=5s",
		"user@dominio:" + senhaSecreta + "@unix(/var/run/mysqld/mysqld.sock)/",
		"user:" + senhaSecreta + "/x@tcp(h:3306)/db",
	}

	for _, raw := range dsns {
		normalized, err := NormalizeDSN(raw)
		require.NoError(t, err)
		parsed, err := mysql.ParseDSN(normalized)
		require.NoError(t, err, raw)
		require.NotEmpty(t, parsed.Passwd)

		redacted := redact(t, raw)
		require.NotContains(t, redacted, parsed.Passwd, "password leaked: %q", redacted)
		require.Contains(t, redacted, parsed.User+":***@", "user lost: %q", redacted)
		require.Contains(t, redacted, parsed.Addr, "address lost: %q", redacted)
	}
}

// Validate errors must not include the full DSN — error messages go to the
// log and, in some setups, to aggregation systems.
func TestValidateErrorDoesNotLeakPassword(t *testing.T) {
	cfg := &Config{}
	cfg.DataSource.Name = "mariadb://user:" + senhaSecreta + "@tcp(localhost:3306)/"
	cfg.Collectors.TableStatLimit = -1 // forces a validation error

	err := cfg.Validate()
	require.Error(t, err)
	require.NotContains(t, err.Error(), senhaSecreta,
		"the password leaked in the validation error message: %q", err.Error())
}

// NormalizeDSN returns the DSN in plaintext (that's what goes to the
// driver), but its error must not echo the full DSN.
func TestNormalizeDSNErrorDoesNotLeakPassword(t *testing.T) {
	// An empty DSN is the only error path; confirms the message is generic.
	_, err := NormalizeDSN("")
	require.Error(t, err)
	require.NotContains(t, strings.ToLower(err.Error()), "senha")
}

// The normalized DSN must preserve the password intact — otherwise the
// connection fails. This test is the counterpart of the previous ones:
// masking is for logging, not for use.
func TestNormalizeDSNPreservesPassword(t *testing.T) {
	dsn, err := NormalizeDSN("mariadb://user:" + senhaSecreta + "@tcp(localhost:3306)/")
	require.NoError(t, err)
	require.Contains(t, dsn, senhaSecreta,
		"the password needs to survive normalization, otherwise the connection fails")
	require.Equal(t, "user:"+senhaSecreta+"@tcp(localhost:3306)/", dsn)
}

// PMM's ConstLabels go into every metric exposed at /metrics, which is an
// unauthenticated endpoint. No PMM field should contain the DSN.
func TestPMMConstLabelsDoNotCarryCredentials(t *testing.T) {
	pmm := PMM{
		ServiceName: "mariadb-01",
		Cluster:     "prod",
		Environment: "production",
	}

	for k, v := range pmm.ConstLabels() {
		require.NotContains(t, v, senhaSecreta, "label %q carries a credential", k)
		require.NotContains(t, v, "@tcp(", "label %q appears to contain a DSN", k)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
