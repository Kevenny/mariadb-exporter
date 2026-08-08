package exporter

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-kit/log"
	"github.com/stretchr/testify/require"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		name      string
		full      string
		comment   string
		wantMajor int
		wantMinor int
		wantPatch int
		isMariaDB bool
	}{
		{
			name:      "MariaDB 11.4 community",
			full:      "11.4.3-MariaDB",
			comment:   "mariadb.org binary distribution",
			wantMajor: 11, wantMinor: 4, wantPatch: 3,
			isMariaDB: true,
		},
		{
			name:      "MariaDB 10.11 com sufixo de distribuição",
			full:      "10.11.6-MariaDB-1:10.11.6+maria~ubu2204",
			comment:   "mariadb.org binary distribution",
			wantMajor: 10, wantMinor: 11, wantPatch: 6,
			isMariaDB: true,
		},
		{
			// O MariaDB pode prefixar a versão com 5.5.5- para clientes legados;
			// a versão real vem depois do prefixo.
			name:      "prefixo de compatibilidade 5.5.5",
			full:      "5.5.5-10.6.12-MariaDB",
			comment:   "MariaDB Server",
			wantMajor: 10, wantMinor: 6, wantPatch: 12,
			isMariaDB: true,
		},
		{
			name:      "MariaDB detectado apenas pelo comment",
			full:      "10.5.20",
			comment:   "MariaDB Server",
			wantMajor: 10, wantMinor: 5, wantPatch: 20,
			isMariaDB: true,
		},
		{
			name:      "MySQL 8.0 não é MariaDB",
			full:      "8.0.36",
			comment:   "MySQL Community Server - GPL",
			wantMajor: 8, wantMinor: 0, wantPatch: 36,
			isMariaDB: false,
		},
		{
			name:      "MySQL 5.7 não é MariaDB",
			full:      "5.7.44-log",
			comment:   "MySQL Community Server (GPL)",
			wantMajor: 5, wantMinor: 7, wantPatch: 44,
			isMariaDB: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := ParseVersion(tc.full, tc.comment)

			require.Equal(t, tc.isMariaDB, info.IsMariaDB)
			require.Equal(t, tc.wantMajor, info.Major)
			require.Equal(t, tc.wantMinor, info.Minor)
			require.Equal(t, tc.wantPatch, info.Patch)
			require.Equal(t, tc.full, info.Full)
		})
	}
}

func TestDetectVersionMariaDB(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT VERSION\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"VERSION()"}).AddRow("11.4.3-MariaDB"))
	mock.ExpectQuery("SELECT @@global.version_comment").
		WillReturnRows(sqlmock.NewRows([]string{"@@global.version_comment"}).AddRow("mariadb.org binary distribution"))

	info, err := DetectVersion(context.Background(), db)
	require.NoError(t, err)
	require.True(t, info.IsMariaDB)
	require.Equal(t, "11.4.3", info.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

// Conectar contra MySQL deve falhar com erro descritivo (seções 2.1 e 20).
func TestDetectVersionRejectsMySQL(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT VERSION\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"VERSION()"}).AddRow("8.0.36"))
	mock.ExpectQuery("SELECT @@global.version_comment").
		WillReturnRows(sqlmock.NewRows([]string{"@@global.version_comment"}).AddRow("MySQL Community Server - GPL"))

	_, err = DetectVersion(context.Background(), db)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotMariaDB)

	// A mensagem precisa ser acionável para quem está operando.
	require.Contains(t, err.Error(), "8.0.36")
	require.Contains(t, err.Error(), "mysqld_exporter")
}

// version_comment ausente não deve impedir a detecção.
func TestDetectVersionWithoutVersionComment(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT VERSION\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"VERSION()"}).AddRow("11.4.3-MariaDB"))
	mock.ExpectQuery("SELECT @@global.version_comment").
		WillReturnError(errors.New("unknown system variable"))

	info, err := DetectVersion(context.Background(), db)
	require.NoError(t, err)
	require.True(t, info.IsMariaDB)
	require.Equal(t, "11.4.3", info.String())
}

func TestDetectVersionQueryError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT VERSION\\(\\)").WillReturnError(errors.New("connection refused"))

	_, err = DetectVersion(context.Background(), db)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotMariaDB, "falha de conexão não é o mesmo que instância incompatível")
}

// Refresh deve ligar as flags dos plugins ACTIVE e ignorar os demais.
func TestFeatureDetectorRefresh(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("FROM information_schema.plugins").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "plugin_status"}).
			AddRow("QUERY_RESPONSE_TIME", "ACTIVE").
			AddRow("METADATA_LOCK_INFO", "ACTIVE").
			AddRow("DISKS", "DISABLED"))

	mock.ExpectQuery("SELECT @@global.query_response_time_stats").
		WillReturnRows(sqlmock.NewRows([]string{"@@global.query_response_time_stats"}).AddRow("ON"))

	mock.ExpectQuery("VARIABLE_NAME = 'userstat'").
		WillReturnRows(sqlmock.NewRows([]string{"VARIABLE_VALUE"}).AddRow("ON"))

	mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'wsrep_ready'").
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("wsrep_ready", "OFF"))

	mock.ExpectQuery("SELECT @@global.gtid_slave_pos").
		WillReturnRows(sqlmock.NewRows([]string{"@@global.gtid_slave_pos"}).AddRow(""))
	mock.ExpectQuery("SHOW ALL SLAVES STATUS").
		WillReturnRows(sqlmock.NewRows([]string{"Master_Host"}))

	version := ParseVersion("11.4.3-MariaDB", "mariadb.org binary distribution")
	d := NewFeatureDetector(db, version, log.NewNopLogger())
	d.Refresh(context.Background())

	f := d.Features()
	require.True(t, f.HasUserStat)
	require.True(t, f.HasQueryResponseTime)
	require.True(t, f.HasMetadataLockInfo)
	require.False(t, f.HasDisksPlugin, "plugin DISABLED não conta como disponível")
	require.False(t, f.HasGalera)
	require.False(t, f.IsReplica)
}

// Plugin instalado mas com a coleta desligada não deve contar como disponível.
func TestFeatureDetectorQueryResponseTimeStatsOff(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("FROM information_schema.plugins").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "plugin_status"}).
			AddRow("QUERY_RESPONSE_TIME", "ACTIVE"))
	mock.ExpectQuery("SELECT @@global.query_response_time_stats").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("OFF"))
	mock.ExpectQuery("VARIABLE_NAME = 'userstat'").
		WillReturnRows(sqlmock.NewRows([]string{"VARIABLE_VALUE"}).AddRow("OFF"))
	mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'wsrep_ready'").
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}))
	mock.ExpectQuery("SELECT @@global.gtid_slave_pos").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(""))
	mock.ExpectQuery("SHOW ALL SLAVES STATUS").
		WillReturnRows(sqlmock.NewRows([]string{"Master_Host"}))

	d := NewFeatureDetector(db, ParseVersion("11.4.3-MariaDB", ""), log.NewNopLogger())
	d.Refresh(context.Background())

	require.False(t, d.Features().HasQueryResponseTime)
	require.False(t, d.Features().HasUserStat)
}

// METADATA_LOCK_INFO exige MariaDB >= 10.0.7 (seção 2.2).
func TestFeatureDetectorMetadataLockVersionGate(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("FROM information_schema.plugins").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "plugin_status"}).
			AddRow("METADATA_LOCK_INFO", "ACTIVE"))
	mock.ExpectQuery("VARIABLE_NAME = 'userstat'").
		WillReturnRows(sqlmock.NewRows([]string{"VARIABLE_VALUE"}).AddRow("OFF"))
	mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'wsrep_ready'").
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}))
	mock.ExpectQuery("SELECT @@global.gtid_slave_pos").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(""))
	mock.ExpectQuery("SHOW ALL SLAVES STATUS").
		WillReturnRows(sqlmock.NewRows([]string{"Master_Host"}))

	// 10.0.6 é anterior ao mínimo exigido.
	d := NewFeatureDetector(db, ParseVersion("10.0.6-MariaDB", ""), log.NewNopLogger())
	d.Refresh(context.Background())

	require.False(t, d.Features().HasMetadataLockInfo)
}

// Erro em uma das consultas não deve zerar as demais features.
func TestFeatureDetectorPartialFailure(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("FROM information_schema.plugins").
		WillReturnError(errors.New("access denied"))
	mock.ExpectQuery("VARIABLE_NAME = 'userstat'").
		WillReturnRows(sqlmock.NewRows([]string{"VARIABLE_VALUE"}).AddRow("ON"))
	mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'wsrep_ready'").
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}))
	mock.ExpectQuery("SELECT @@global.gtid_slave_pos").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("0-1-100"))

	d := NewFeatureDetector(db, ParseVersion("11.4.3-MariaDB", ""), log.NewNopLogger())
	d.Refresh(context.Background())

	f := d.Features()
	require.True(t, f.HasUserStat, "falha na consulta de plugins não deve afetar userstat")
	require.True(t, f.IsReplica)
	require.False(t, f.HasQueryResponseTime)
}

func TestIsTruthy(t *testing.T) {
	for _, v := range []string{"ON", "on", "1", "YES", "true", "ALL", " ACTIVE "} {
		require.True(t, isTruthy(v), "%q deveria ser verdadeiro", v)
	}
	for _, v := range []string{"OFF", "0", "NO", "false", "", "DEMAND"} {
		require.False(t, isTruthy(v), "%q deveria ser falso", v)
	}
}
