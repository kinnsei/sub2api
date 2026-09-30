//go:build unit

package service

import (
	"context"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/stretchr/testify/require"
)

// recordingQueryMatcher accepts every query and records it in order so a test
// can assert on the exact SQL ent/sqlmock never normally exposes.
type recordingQueryMatcher struct {
	seen *[]string
}

func (m recordingQueryMatcher) Match(_, actual string) error {
	*m.seen = append(*m.seen, actual)
	return nil
}

func newPostgresSQLMockClient(t *testing.T) (*dbent.Client, sqlmock.Sqlmock, *[]string) {
	t.Helper()
	seen := &[]string{}
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(recordingQueryMatcher{seen: seen}))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = client.Close() })
	return client, mock, seen
}

func containsSQL(queries []string, needle string) bool {
	for _, q := range queries {
		if strings.Contains(strings.ToUpper(q), strings.ToUpper(needle)) {
			return true
		}
	}
	return false
}

// The pending-order cap is a read-then-insert sequence inside one transaction.
// Without a lock two concurrent creators can both pass the count check, so the
// count must be preceded by a per-user transaction-scoped lock.
func TestCheckPendingLimitSerializesPerUserOnPostgres(t *testing.T) {
	client, mock, seen := newPostgresSQLMockClient(t)
	ctx := context.Background()

	mock.ExpectBegin()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	// Rows shaped like the count query so the pre-fix code path (no lock) still
	// runs to completion and fails on the ordering assertion below rather than
	// on a driver scan error.
	mock.ExpectQuery(".*").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(".*").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	svc := &PaymentService{entClient: client}
	require.NoError(t, svc.checkPendingLimit(ctx, tx, 4242, 3))

	require.NotEmpty(t, *seen, "expected the limit check to issue SQL")
	lockIdx, countIdx := -1, -1
	for i, q := range *seen {
		upper := strings.ToUpper(q)
		if strings.Contains(upper, "PG_ADVISORY_XACT_LOCK") {
			lockIdx = i
		}
		if strings.Contains(upper, "COUNT(") {
			countIdx = i
		}
	}
	require.GreaterOrEqual(t, lockIdx, 0, "pending cap check must take a transaction-scoped advisory lock, got %v", *seen)
	require.GreaterOrEqual(t, countIdx, 0, "pending cap check must still count pending orders, got %v", *seen)
	require.Less(t, lockIdx, countIdx,
		"the per-user quota lock must be acquired before the count, otherwise the TOCTOU window stays open")
}

// The lock key must be per user, so unrelated users do not serialize.
func TestPaymentOrderUserQuotaLockKeyIsPerUser(t *testing.T) {
	require.NotEqual(t, paymentOrderUserQuotaLockKey(1), paymentOrderUserQuotaLockKey(2))
	require.Equal(t, hashAdvisoryLockID(paymentOrderUserQuotaLockKey(1)), hashAdvisoryLockID(paymentOrderUserQuotaLockKey(1)))
	require.NotEqual(t, hashAdvisoryLockID(paymentOrderUserQuotaLockKey(1)), hashAdvisoryLockID(paymentOrderUserQuotaLockKey(2)))
}

// SQLite has no advisory locks, so the guard must be a silent no-op there or
// every unit test that creates an order would fail.
func TestLockPaymentOrderUserQuotaIsNoOpOnSQLite(t *testing.T) {
	client := newPaymentConfigServiceTestClient(t)
	ctx := context.Background()

	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	require.NoError(t, lockPaymentOrderUserQuota(ctx, tx, 99))
	require.NoError(t, lockPaymentOrderUserQuota(ctx, nil, 99))
	require.NoError(t, lockPaymentOrderUserQuota(ctx, tx, 0))
}
