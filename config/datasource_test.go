package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// writeCertPair writes a self-signed certificate and its key as PEM files.
func writeCertPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPath = writeFile(t, "cert.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	keyPath = writeFile(t, "key.pem", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	return certPath, keyPath
}

func TestDriverConfigFromDSN(t *testing.T) {
	cfg, err := DataSource{Name: "mariadb://pmm:pw@tcp(db:3307)/?timeout=5s"}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, "pmm", cfg.User)
	require.Equal(t, "pw", cfg.Passwd)
	require.Equal(t, "db:3307", cfg.Addr)
	require.Equal(t, 5*time.Second, cfg.Timeout)
	require.Nil(t, cfg.TLS)
}

func TestDriverConfigFromMyCnf(t *testing.T) {
	path := writeFile(t, "my.cnf", `
# comment
[mysqld]
user = mysql

[client]
user = exporter
password = "p#ss word"
host = db.internal
port = 3310

[client-mariadb]
password = 'override # kept'
`)
	cfg, err := DataSource{MyCnf: path}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, "exporter", cfg.User, "[mysqld] must be ignored")
	require.Equal(t, "override # kept", cfg.Passwd, "later client section wins; quoted '#' is literal")
	require.Equal(t, "tcp", cfg.Net)
	require.Equal(t, "db.internal:3310", cfg.Addr)
}

func TestMyCnfSocketAndDefaults(t *testing.T) {
	path := writeFile(t, "my.cnf", "[client]\nuser=exporter\nsocket=/run/mysqld/mysqld.sock\n")
	cfg, err := DataSource{MyCnf: path}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, "unix", cfg.Net)
	require.Equal(t, "/run/mysqld/mysqld.sock", cfg.Addr)

	path = writeFile(t, "my2.cnf", "[client]\nuser=exporter # inline comment\n")
	cfg, err = DataSource{MyCnf: path}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, "exporter", cfg.User)
	require.Equal(t, "localhost:3306", cfg.Addr)
}

func TestMyCnfErrors(t *testing.T) {
	_, err := DataSource{MyCnf: writeFile(t, "a.cnf", "[client]\npassword=x\n")}.DriverConfig()
	require.ErrorContains(t, err, "no user")

	_, err = DataSource{MyCnf: writeFile(t, "b.cnf", "[client]\nuser=u\nport=abc\n")}.DriverConfig()
	require.ErrorContains(t, err, "invalid port")

	_, err = DataSource{MyCnf: writeFile(t, "c.cnf", "[client\nuser=u\n")}.DriverConfig()
	require.ErrorContains(t, err, "malformed section")
}

// An error about the my.cnf must not carry the password it contains.
func TestMyCnfErrorDoesNotLeakPassword(t *testing.T) {
	path := writeFile(t, "my.cnf", "[client]\npassword="+senhaSecreta+"\nport=abc\nuser=u\n")
	_, err := DataSource{MyCnf: path}.DriverConfig()
	require.Error(t, err)
	require.NotContains(t, err.Error(), senhaSecreta)
}

func TestPasswordFileOverrides(t *testing.T) {
	pw := writeFile(t, "pw", " spaced secret \n")

	cfg, err := DataSource{Name: "u:old@tcp(h:3306)/", PasswordFile: pw}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, " spaced secret ", cfg.Passwd, "only the trailing newline is stripped")

	cnf := writeFile(t, "my.cnf", "[client]\nuser=u\npassword=old\n")
	cfg, err = DataSource{MyCnf: cnf, PasswordFile: pw}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, " spaced secret ", cfg.Passwd)

	crlf := writeFile(t, "pw-crlf", "secret\r\n")
	cfg, err = DataSource{Name: "u@tcp(h:3306)/", PasswordFile: crlf}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, "secret", cfg.Passwd)

	_, err = DataSource{Name: "u@tcp(h:3306)/", PasswordFile: writeFile(t, "empty", "\n")}.DriverConfig()
	require.ErrorContains(t, err, "empty")
}

func TestTLSFromFlags(t *testing.T) {
	certPath, keyPath := writeCertPair(t)

	cfg, err := DataSource{
		Name: "u:p@tcp(db.internal:3306)/?tls=preferred",
		TLS:  TLS{CA: certPath, Cert: certPath, Key: keyPath},
	}.DriverConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg.TLS)
	require.NotNil(t, cfg.TLS.RootCAs)
	require.Len(t, cfg.TLS.Certificates, 1)
	require.Equal(t, uint16(tls.VersionTLS12), cfg.TLS.MinVersion)
	require.False(t, cfg.TLS.InsecureSkipVerify)
	require.False(t, cfg.AllowFallbackToPlaintext, "explicit TLS must not fall back to plaintext")
}

func TestTLSFromMyCnfAndFlagPrecedence(t *testing.T) {
	certPath, keyPath := writeCertPair(t)
	cnf := writeFile(t, "my.cnf", "[client]\nuser=u\nssl-ca=/does/not/exist.pem\nssl_cert="+certPath+"\nssl-key="+keyPath+"\n")

	_, err := DataSource{MyCnf: cnf}.DriverConfig()
	require.ErrorContains(t, err, "TLS CA", "ssl-ca from my.cnf is used")

	cfg, err := DataSource{MyCnf: cnf, TLS: TLS{CA: certPath}}.DriverConfig()
	require.NoError(t, err, "--tls.ca overrides ssl-ca")
	require.Len(t, cfg.TLS.Certificates, 1, "client cert from my.cnf is kept")

	cfg, err = DataSource{MyCnf: writeFile(t, "ssl.cnf", "[client]\nuser=u\nssl\n")}.DriverConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg.TLS, "bare 'ssl' enables TLS with the system roots")
	require.Nil(t, cfg.TLS.RootCAs)
}

func TestTLSErrors(t *testing.T) {
	notPEM := writeFile(t, "ca.pem", "not a certificate")
	_, err := DataSource{Name: "u@tcp(h:3306)/", TLS: TLS{CA: notPEM}}.DriverConfig()
	require.ErrorContains(t, err, "no PEM certificate")

	certPath, _ := writeCertPair(t)
	_, err = DataSource{Name: "u@tcp(h:3306)/", TLS: TLS{Cert: certPath}}.DriverConfig()
	require.ErrorContains(t, err, "together")
}

func TestValidateDataSourceSources(t *testing.T) {
	cfg := &Config{}
	cfg.DataSource.MyCnf = "/etc/mariadb_exporter/my.cnf"
	require.NoError(t, cfg.Validate(), "my.cnf alone is enough")

	cfg.DataSource.Name = "u:p@tcp(h:3306)/"
	require.ErrorContains(t, cfg.Validate(), "mutually exclusive")

	cfg = &Config{}
	cfg.DataSource.Name = "u:p@tcp(h:3306)/"
	cfg.DataSource.TLS.Key = "/k.pem"
	require.ErrorContains(t, cfg.Validate(), "together")
}

func TestRedactedMarksCustomTLS(t *testing.T) {
	certPath, _ := writeCertPair(t)
	cfg, err := DataSource{Name: "u:" + senhaSecreta + "@tcp(h:3306)/", TLS: TLS{CA: certPath}}.DriverConfig()
	require.NoError(t, err)
	out := Redacted(cfg)
	require.NotContains(t, out, senhaSecreta)
	require.Contains(t, out, "tls=custom")
	require.Nil(t, cfg.TLS.Certificates, "Redacted must not mutate the config")
	require.Equal(t, senhaSecreta, cfg.Passwd, "Redacted must not mutate the config")
}

// --datasource.timeout becomes the server-side max_statement_time, unless the
// DSN sets it explicitly.
func TestMaxStatementTime(t *testing.T) {
	cfg, err := DataSource{Name: "u@tcp(h:3306)/", Timeout: 30 * time.Second}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, "30", cfg.Params["max_statement_time"])

	cfg, err = DataSource{Name: "u@tcp(h:3306)/", Timeout: 1500 * time.Millisecond}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, "1.5", cfg.Params["max_statement_time"])

	cfg, err = DataSource{Name: "u@tcp(h:3306)/?max_statement_time=0", Timeout: 30 * time.Second}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, "0", cfg.Params["max_statement_time"], "the DSN wins")

	cnf := writeFile(t, "my.cnf", "[client]\nuser=u\n")
	cfg, err = DataSource{MyCnf: cnf, Timeout: 10 * time.Second}.DriverConfig()
	require.NoError(t, err)
	require.Equal(t, "10", cfg.Params["max_statement_time"])
}
