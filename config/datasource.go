package config

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// DriverConfig builds the driver configuration from the DSN or the my.cnf
// file, then applies the password file and the TLS flags. The result goes to
// mysql.NewConnector, so a password never has to be re-encoded into a DSN
// string.
func (ds DataSource) DriverConfig() (*mysql.Config, error) {
	var (
		cfg    *mysql.Config
		cnfTLS TLS
		err    error
	)

	if strings.TrimSpace(ds.Name) != "" {
		dsn, err := NormalizeDSN(ds.Name)
		if err != nil {
			return nil, err
		}
		// The driver's parse errors never echo the DSN, so they are safe to log.
		if cfg, err = mysql.ParseDSN(dsn); err != nil {
			return nil, fmt.Errorf("invalid DSN: %w", err)
		}
	} else {
		if cfg, cnfTLS, err = loadMyCnf(ds.MyCnf); err != nil {
			return nil, fmt.Errorf("--config.my-cnf %q: %w", ds.MyCnf, err)
		}
	}

	if ds.PasswordFile != "" {
		if cfg.Passwd, err = readPasswordFile(ds.PasswordFile); err != nil {
			return nil, fmt.Errorf("--datasource.password-file %q: %w", ds.PasswordFile, err)
		}
	}

	// Explicit flags win over the ssl-* options of my.cnf, field by field.
	t := cnfTLS
	if ds.TLS.CA != "" {
		t.CA = ds.TLS.CA
	}
	if ds.TLS.Cert != "" || ds.TLS.Key != "" {
		t.Cert, t.Key = ds.TLS.Cert, ds.TLS.Key
	}
	t.InsecureSkipVerify = t.InsecureSkipVerify || ds.TLS.InsecureSkipVerify

	if t.enabled() {
		if cfg.TLS, err = buildTLS(t); err != nil {
			return nil, err
		}
		// A tls=preferred in the DSN would otherwise silently fall back to
		// plaintext when the operator explicitly asked for TLS.
		cfg.AllowFallbackToPlaintext = false
	}

	// Canceling a scrape only closes the client side: the server keeps running
	// the query to the end. max_statement_time (MariaDB >= 10.1) makes the
	// server abort it, so slow queries from abandoned scrapes can't pile up.
	// A value given in the DSN wins.
	if _, set := cfg.Params[maxStatementTimeParam]; !set && ds.Timeout > 0 {
		if cfg.Params == nil {
			cfg.Params = map[string]string{}
		}
		cfg.Params[maxStatementTimeParam] = strconv.FormatFloat(ds.Timeout.Seconds(), 'f', -1, 64)
	}

	return cfg, nil
}

const maxStatementTimeParam = "max_statement_time"

// Redacted renders the configuration as a DSN with the password masked, for
// logging. It works from the parsed config, so no password format can
// confuse it.
func Redacted(cfg *mysql.Config) string {
	c := cfg.Clone()
	if c.Passwd != "" {
		c.Passwd = "***"
	}
	if c.TLS != nil && c.TLSConfig == "" {
		c.TLSConfig = "custom"
	}
	return c.FormatDSN()
}

func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Only the line terminator an editor or `echo` leaves behind is removed:
	// leading/trailing spaces may be part of the password.
	pw := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if pw == "" {
		return "", fmt.Errorf("file is empty")
	}
	return pw, nil
}

func buildTLS(t TLS) (*tls.Config, error) {
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: t.InsecureSkipVerify, //nolint:gosec // opt-in via --tls.insecure-skip-verify
	}

	if t.CA != "" {
		pem, err := os.ReadFile(t.CA)
		if err != nil {
			return nil, fmt.Errorf("TLS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("TLS CA %q: no PEM certificate found", t.CA)
		}
		tc.RootCAs = pool
	}

	if t.Cert != "" || t.Key != "" {
		if t.Cert == "" || t.Key == "" {
			return nil, fmt.Errorf("TLS client certificate and key must be used together")
		}
		pair, err := tls.LoadX509KeyPair(t.Cert, t.Key)
		if err != nil {
			return nil, fmt.Errorf("TLS client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{pair}
	}

	return tc, nil
}

// myCnfSections are read in order; a later section overrides an earlier one,
// as the MariaDB client does.
var myCnfSections = []string{"client", "client-mariadb", "mariadb-client"}

// loadMyCnf reads the connection settings from a my.cnf-style file:
// user, password, host, port, socket and the ssl-* options.
func loadMyCnf(path string) (*mysql.Config, TLS, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, TLS{}, err
	}

	values, err := parseMyCnf(data)
	if err != nil {
		return nil, TLS{}, err
	}

	cfg := mysql.NewConfig()
	cfg.User = values["user"]
	cfg.Passwd = values["password"]

	if socket := values["socket"]; socket != "" && values["host"] == "" {
		cfg.Net, cfg.Addr = "unix", socket
	} else {
		host := values["host"]
		if host == "" {
			host = "localhost"
		}
		port := values["port"]
		if port == "" {
			port = "3306"
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return nil, TLS{}, fmt.Errorf("invalid port %q", port)
		}
		cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(host, port)
	}

	if cfg.User == "" {
		return nil, TLS{}, fmt.Errorf("no user in sections %v", myCnfSections)
	}

	t := TLS{CA: values["ssl-ca"], Cert: values["ssl-cert"], Key: values["ssl-key"]}
	if !t.enabled() && isTrue(values["ssl"]) {
		// ssl without a CA: TLS verified against the system roots.
		return cfg, TLS{}, withSystemTLS(cfg)
	}
	return cfg, t, nil
}

func withSystemTLS(cfg *mysql.Config) error {
	tc, err := buildTLS(TLS{})
	if err != nil {
		return err
	}
	cfg.TLS = tc
	return nil
}

func parseMyCnf(data []byte) (map[string]string, error) {
	wanted := make(map[string]bool, len(myCnfSections))
	for _, s := range myCnfSections {
		wanted[s] = true
	}

	values := map[string]string{}
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return nil, fmt.Errorf("line %d: malformed section header", lineNo)
			}
			section = strings.ToLower(strings.TrimSpace(line[1:end]))
			continue
		}
		if !wanted[section] {
			continue
		}

		key, value, hasValue := strings.Cut(line, "=")
		// Option names accept '-' and '_' interchangeably.
		key = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "_", "-")
		if !hasValue {
			values[key] = "1"
			continue
		}
		values[key] = unquoteCnfValue(strings.TrimSpace(value))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// unquoteCnfValue strips matching quotes; an unquoted value ends at an
// inline '#' comment, while a quoted one keeps '#' literally.
func unquoteCnfValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}
