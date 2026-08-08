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
			name:      "MariaDB 10.11 with distribution suffix",
			full:      "10.11.6-MariaDB-1:10.11.6+maria~ubu2204",
			comment:   "mariadb.org binary distribution",
			wantMajor: 10, wantMinor: 11, wantPatch: 6,
			isMariaDB: true,
		},
		{
			// MariaDB may prefix the version with 5.5.5- for legacy clients;
			// the real version comes after the prefix.
			name:      "5.5.5 compatibility prefix",
			full:      "5.5.5-10.6.12-MariaDB",
			comment:   "MariaDB Server",
			wantMajor: 10, wantMinor: 6, wantPatch: 12,
			isMariaDB: true,
		},
		{
			name:      "MariaDB detected only from the comment",
			full:      "10.5.20",
			comment:   "MariaDB Server",
			wantMajor: 10, wantMinor: 5, wantPatch: 20,
			isMariaDB: true,
		},
		{
			name:      "MySQL 8.0 is not MariaDB",
			full:      "8.0.36",
			comment:   "MySQL Community Server - GPL",
			wantMajor: 8, wantMinor: 0, wantPatch: 36,
			isMariaDB: false,
		},
		{
			name:      "MySQL 5.7 is not MariaDB",
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

// Connecting against MySQL must fail with a descriptive error (sections 2.1 and 20).
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

	// The message needs to be actionable for whoever is operating it.
	require.Contains(t, err.Error(), "8.0.36")
	require.Contains(t, err.Error(), "mysqld_exporter")
}

// A missing version_comment should not prevent detection.
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
	require.NotErrorIs(t, err, ErrNotMariaDB, "connection failure is not the same as incompatible instance")
}

// Refresh should turn on the flags for ACTIVE plugins and ignore the rest.
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
	require.False(t, f.HasDisksPlugin, "a DISABLED plugin does not count as available")
	require.False(t, f.HasGalera)
	require.False(t, f.IsReplica)
}

// A plugin installed but with collection turned off should not count as available.
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

// METADATA_LOCK_INFO requires MariaDB >= 10.0.7 (section 2.2).
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

	// 10.0.6 is earlier than the required minimum.
	d := NewFeatureDetector(db, ParseVersion("10.0.6-MariaDB", ""), log.NewNopLogger())
	d.Refresh(context.Background())

	require.False(t, d.Features().HasMetadataLockInfo)
}

// An error in one of the queries should not zero out the other features.
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
	require.True(t, f.HasUserStat, "a failure querying plugins should not affect userstat")
	require.True(t, f.IsReplica)
	require.False(t, f.HasQueryResponseTime)
}

func TestIsTruthy(t *testing.T) {
	for _, v := range []string{"ON", "on", "1", "YES", "true", "ALL", " ACTIVE "} {
		require.True(t, isTruthy(v), "%q should be true", v)
	}
	for _, v := range []string{"OFF", "0", "NO", "false", "", "DEMAND"} {
		require.False(t, isTruthy(v), "%q should be false", v)
	}
}
