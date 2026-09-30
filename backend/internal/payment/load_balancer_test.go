//go:build unit

package payment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
)

func TestInstanceSupportsType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		supportedTypes string
		target         PaymentType
		expected       bool
	}{
		{
			name:           "exact match single type",
			supportedTypes: "alipay",
			target:         "alipay",
			expected:       true,
		},
		{
			name:           "no match single type",
			supportedTypes: "wxpay",
			target:         "alipay",
			expected:       false,
		},
		{
			name:           "match in comma-separated list",
			supportedTypes: "alipay,wxpay,stripe",
			target:         "wxpay",
			expected:       true,
		},
		{
			name:           "first in comma-separated list",
			supportedTypes: "alipay,wxpay",
			target:         "alipay",
			expected:       true,
		},
		{
			name:           "last in comma-separated list",
			supportedTypes: "alipay,wxpay,stripe",
			target:         "stripe",
			expected:       true,
		},
		{
			name:           "no match in comma-separated list",
			supportedTypes: "alipay,wxpay",
			target:         "stripe",
			expected:       false,
		},
		{
			name:           "empty target",
			supportedTypes: "alipay,wxpay",
			target:         "",
			expected:       false,
		},
		{
			name:           "types with spaces are trimmed",
			supportedTypes: " alipay , wxpay ",
			target:         "alipay",
			expected:       true,
		},
		{
			name:           "legacy alipay direct supports canonical visible method",
			supportedTypes: "alipay_direct",
			target:         "alipay",
			expected:       true,
		},
		{
			name:           "legacy wxpay direct supports canonical visible method",
			supportedTypes: "wxpay_direct",
			target:         "wxpay",
			expected:       true,
		},
		{
			name:           "empty supported types means all supported",
			supportedTypes: "",
			target:         "alipay",
			expected:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := InstanceSupportsType(tt.supportedTypes, tt.target)
			if got != tt.expected {
				t.Fatalf("InstanceSupportsType(%q, %q) = %v, want %v", tt.supportedTypes, tt.target, got, tt.expected)
			}
		})
	}
}

func TestGetInstanceChannelLimitsFallsBackToLegacyDirectAliases(t *testing.T) {
	t.Parallel()

	inst := testInstance(1, TypeAlipay, makeLimitsJSON(TypeAlipayDirect, ChannelLimits{SingleMax: 66}))
	got, ok := getInstanceChannelLimits(inst, TypeAlipay)
	if !ok || got.SingleMax != 66 {
		t.Fatalf("getInstanceChannelLimits() = %+v ok=%v, want SingleMax=66 ok=true", got, ok)
	}

	wxInst := testInstance(2, TypeWxpay, makeLimitsJSON(TypeWxpayDirect, ChannelLimits{SingleMin: 8}))
	wxGot, ok := getInstanceChannelLimits(wxInst, TypeWxpay)
	if !ok || wxGot.SingleMin != 8 {
		t.Fatalf("getInstanceChannelLimits() = %+v ok=%v, want SingleMin=8 ok=true", wxGot, ok)
	}
}

// ---------------------------------------------------------------------------
// Helper to build test PaymentProviderInstance values
// ---------------------------------------------------------------------------

func testInstance(id int64, providerKey, limits string) *dbent.PaymentProviderInstance {
	return &dbent.PaymentProviderInstance{
		ID:          id,
		ProviderKey: providerKey,
		Limits:      limits,
		Enabled:     true,
	}
}

// makeLimitsJSON builds a limits JSON string for a single payment type.
func makeLimitsJSON(paymentType string, cl ChannelLimits) string {
	m := map[string]ChannelLimits{paymentType: cl}
	b, _ := json.Marshal(m)
	return string(b)
}

// ---------------------------------------------------------------------------
// filterByLimits
// ---------------------------------------------------------------------------

func TestFilterByLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		candidates  []instanceCandidate
		paymentType PaymentType
		orderAmount float64
		wantIDs     []int64 // expected surviving instance IDs
	}{
		{
			name: "order below SingleMin is filtered out",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMin: 10})), dailyUsed: 0},
			},
			paymentType: "alipay",
			orderAmount: 5,
			wantIDs:     nil,
		},
		{
			name: "order at exact SingleMin boundary passes",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMin: 10})), dailyUsed: 0},
			},
			paymentType: "alipay",
			orderAmount: 10,
			wantIDs:     []int64{1},
		},
		{
			name: "order above SingleMax is filtered out",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMax: 100})), dailyUsed: 0},
			},
			paymentType: "alipay",
			orderAmount: 150,
			wantIDs:     nil,
		},
		{
			name: "order at exact SingleMax boundary passes",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMax: 100})), dailyUsed: 0},
			},
			paymentType: "alipay",
			orderAmount: 100,
			wantIDs:     []int64{1},
		},
		{
			name: "daily used + orderAmount exceeding dailyLimit is filtered out",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{DailyLimit: 500})), dailyUsed: 480},
			},
			paymentType: "alipay",
			orderAmount: 30,
			wantIDs:     nil, // 480+30=510 > 500
		},
		{
			name: "daily used + orderAmount equal to dailyLimit passes (strict greater-than)",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{DailyLimit: 500})), dailyUsed: 480},
			},
			paymentType: "alipay",
			orderAmount: 20,
			wantIDs:     []int64{1}, // 480+20=500, 500 > 500 is false → passes
		},
		{
			name: "daily used + orderAmount below dailyLimit passes",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{DailyLimit: 500})), dailyUsed: 400},
			},
			paymentType: "alipay",
			orderAmount: 50,
			wantIDs:     []int64{1},
		},
		{
			name: "no limits configured passes through",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", ""), dailyUsed: 99999},
			},
			paymentType: "alipay",
			orderAmount: 100,
			wantIDs:     []int64{1},
		},
		{
			name: "multiple candidates with partial filtering",
			candidates: []instanceCandidate{
				// singleMax=50, order=80 → filtered out
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMax: 50})), dailyUsed: 0},
				// no limits → passes
				{inst: testInstance(2, "easypay", ""), dailyUsed: 0},
				// singleMin=100, order=80 → filtered out
				{inst: testInstance(3, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMin: 100})), dailyUsed: 0},
				// daily limit ok → passes (500+80=580 < 1000)
				{inst: testInstance(4, "easypay", makeLimitsJSON("alipay", ChannelLimits{DailyLimit: 1000})), dailyUsed: 500},
			},
			paymentType: "alipay",
			orderAmount: 80,
			wantIDs:     []int64{2, 4},
		},
		{
			name: "zero SingleMin and SingleMax means no single-transaction limit",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMin: 0, SingleMax: 0, DailyLimit: 0})), dailyUsed: 0},
			},
			paymentType: "alipay",
			orderAmount: 99999,
			wantIDs:     []int64{1},
		},
		{
			name: "all limits combined - order passes all checks",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMin: 10, SingleMax: 200, DailyLimit: 1000})), dailyUsed: 500},
			},
			paymentType: "alipay",
			orderAmount: 50,
			wantIDs:     []int64{1},
		},
		{
			name: "all limits combined - order fails SingleMin",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMin: 10, SingleMax: 200, DailyLimit: 1000})), dailyUsed: 500},
			},
			paymentType: "alipay",
			orderAmount: 5,
			wantIDs:     nil,
		},
		{
			name: "unreadable limits exclude the instance instead of unlocking it",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", "not-json{"), dailyUsed: 0},
			},
			paymentType: "alipay",
			orderAmount: 100,
			wantIDs:     nil,
		},
		{
			name: "unreadable limits on one instance still allow a healthy sibling",
			candidates: []instanceCandidate{
				{inst: testInstance(1, "easypay", "not-json{"), dailyUsed: 0},
				{inst: testInstance(2, "easypay", makeLimitsJSON("alipay", ChannelLimits{SingleMax: 500})), dailyUsed: 0},
			},
			paymentType: "alipay",
			orderAmount: 100,
			wantIDs:     []int64{2},
		},
		{
			name:        "empty candidates returns empty",
			candidates:  nil,
			paymentType: "alipay",
			orderAmount: 10,
			wantIDs:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := filterByLimits(tt.candidates, tt.paymentType, tt.orderAmount)
			gotIDs := make([]int64, len(got))
			for i, c := range got {
				gotIDs[i] = c.inst.ID
			}
			if !int64SliceEqual(gotIDs, tt.wantIDs) {
				t.Fatalf("filterByLimits() returned IDs %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// pickLeastAmount
// ---------------------------------------------------------------------------

func TestPickLeastAmount(t *testing.T) {
	t.Parallel()

	t.Run("picks candidate with lowest dailyUsed", func(t *testing.T) {
		t.Parallel()
		candidates := []instanceCandidate{
			{inst: testInstance(1, "easypay", ""), dailyUsed: 300},
			{inst: testInstance(2, "easypay", ""), dailyUsed: 100},
			{inst: testInstance(3, "easypay", ""), dailyUsed: 200},
		}
		got := pickLeastAmount(candidates)
		if got.inst.ID != 2 {
			t.Fatalf("pickLeastAmount() picked instance %d, want 2", got.inst.ID)
		}
	})

	t.Run("with equal dailyUsed picks the first one", func(t *testing.T) {
		t.Parallel()
		candidates := []instanceCandidate{
			{inst: testInstance(1, "easypay", ""), dailyUsed: 100},
			{inst: testInstance(2, "easypay", ""), dailyUsed: 100},
			{inst: testInstance(3, "easypay", ""), dailyUsed: 200},
		}
		got := pickLeastAmount(candidates)
		if got.inst.ID != 1 {
			t.Fatalf("pickLeastAmount() picked instance %d, want 1 (first with lowest)", got.inst.ID)
		}
	})

	t.Run("single candidate returns that candidate", func(t *testing.T) {
		t.Parallel()
		candidates := []instanceCandidate{
			{inst: testInstance(42, "easypay", ""), dailyUsed: 999},
		}
		got := pickLeastAmount(candidates)
		if got.inst.ID != 42 {
			t.Fatalf("pickLeastAmount() picked instance %d, want 42", got.inst.ID)
		}
	})

	t.Run("zero usage among non-zero picks zero", func(t *testing.T) {
		t.Parallel()
		candidates := []instanceCandidate{
			{inst: testInstance(1, "easypay", ""), dailyUsed: 500},
			{inst: testInstance(2, "easypay", ""), dailyUsed: 0},
			{inst: testInstance(3, "easypay", ""), dailyUsed: 300},
		}
		got := pickLeastAmount(candidates)
		if got.inst.ID != 2 {
			t.Fatalf("pickLeastAmount() picked instance %d, want 2", got.inst.ID)
		}
	})
}

// ---------------------------------------------------------------------------
// getInstanceChannelLimits
// ---------------------------------------------------------------------------

func TestGetInstanceChannelLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		inst        *dbent.PaymentProviderInstance
		paymentType PaymentType
		want        ChannelLimits
		wantOK      bool
	}{
		{
			name:        "empty limits string returns zero ChannelLimits",
			inst:        testInstance(1, "easypay", ""),
			paymentType: "alipay",
			want:        ChannelLimits{},
			wantOK:      true,
		},
		{
			name:        "invalid JSON fails closed instead of unlocking the instance",
			inst:        testInstance(1, "easypay", "not-json{"),
			paymentType: "alipay",
			want:        ChannelLimits{},
			wantOK:      false,
		},
		{
			name: "valid JSON with matching payment type",
			inst: testInstance(1, "easypay",
				`{"alipay":{"singleMin":5,"singleMax":200,"dailyLimit":1000}}`),
			paymentType: "alipay",
			want:        ChannelLimits{SingleMin: 5, SingleMax: 200, DailyLimit: 1000},
			wantOK:      true,
		},
		{
			name: "payment type not in limits returns zero ChannelLimits",
			inst: testInstance(1, "easypay",
				`{"alipay":{"singleMin":5,"singleMax":200}}`),
			paymentType: "wxpay",
			want:        ChannelLimits{},
			wantOK:      true,
		},
		{
			name: "stripe provider uses stripe lookup key regardless of payment type",
			inst: testInstance(1, "stripe",
				`{"stripe":{"singleMin":10,"singleMax":500,"dailyLimit":5000}}`),
			paymentType: "alipay",
			want:        ChannelLimits{SingleMin: 10, SingleMax: 500, DailyLimit: 5000},
			wantOK:      true,
		},
		{
			name: "stripe provider ignores payment type key even if present",
			inst: testInstance(1, "stripe",
				`{"stripe":{"singleMin":10,"singleMax":500},"alipay":{"singleMin":1,"singleMax":100}}`),
			paymentType: "alipay",
			want:        ChannelLimits{SingleMin: 10, SingleMax: 500},
			wantOK:      true,
		},
		{
			name: "non-stripe provider uses payment type as lookup key",
			inst: testInstance(1, "easypay",
				`{"alipay":{"singleMin":5},"wxpay":{"singleMin":10}}`),
			paymentType: "wxpay",
			want:        ChannelLimits{SingleMin: 10},
			wantOK:      true,
		},
		{
			name: "valid JSON with partial limits (only dailyLimit)",
			inst: testInstance(1, "easypay",
				`{"alipay":{"dailyLimit":800}}`),
			paymentType: "alipay",
			want:        ChannelLimits{DailyLimit: 800},
			wantOK:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := getInstanceChannelLimits(tt.inst, tt.paymentType)
			if got != tt.want {
				t.Fatalf("getInstanceChannelLimits() = %+v, want %+v", got, tt.want)
			}
			if ok != tt.wantOK {
				t.Fatalf("getInstanceChannelLimits() ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// startOfDay
// ---------------------------------------------------------------------------

func TestStartOfDay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{
			name: "midday returns midnight of same day",
			in:   time.Date(2025, 6, 15, 14, 30, 45, 123456789, time.UTC),
			want: time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "midnight returns same time",
			in:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			want: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "last second of day returns midnight of same day",
			in:   time.Date(2025, 12, 31, 23, 59, 59, 999999999, time.UTC),
			want: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "preserves timezone location",
			in:   time.Date(2025, 3, 10, 15, 0, 0, 0, time.FixedZone("CST", 8*3600)),
			want: time.Date(2025, 3, 10, 0, 0, 0, 0, time.FixedZone("CST", 8*3600)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := startOfDay(tt.in)
			if !got.Equal(tt.want) {
				t.Fatalf("startOfDay(%v) = %v, want %v", tt.in, got, tt.want)
			}
			// Also verify location is preserved.
			if got.Location().String() != tt.want.Location().String() {
				t.Fatalf("startOfDay() location = %v, want %v", got.Location(), tt.want.Location())
			}
		})
	}
}

func TestDecryptConfig_PlaintextAndLegacyCompat(t *testing.T) {
	t.Parallel()

	key := make([]byte, AES256KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	wrongKey := make([]byte, AES256KeySize)
	for i := range wrongKey {
		wrongKey[i] = byte(0xFF - i)
	}

	plaintextJSON := `{"appId":"app-123","secret":"sec-xyz"}`

	legacyEncrypted, err := Encrypt(plaintextJSON, key)
	if err != nil {
		t.Fatalf("seed Encrypt: %v", err)
	}

	tests := []struct {
		name   string
		stored string
		key    []byte
		want   map[string]string
	}{
		{
			name:   "empty stored returns nil map",
			stored: "",
			key:    key,
			want:   nil,
		},
		{
			name:   "plaintext JSON parses directly",
			stored: plaintextJSON,
			key:    nil,
			want:   map[string]string{"appId": "app-123", "secret": "sec-xyz"},
		},
		{
			name:   "plaintext JSON works even with key present",
			stored: plaintextJSON,
			key:    key,
			want:   map[string]string{"appId": "app-123", "secret": "sec-xyz"},
		},
		{
			name:   "legacy ciphertext with correct key decrypts",
			stored: legacyEncrypted,
			key:    key,
			want:   map[string]string{"appId": "app-123", "secret": "sec-xyz"},
		},
		{
			name:   "legacy ciphertext with no key treated as empty",
			stored: legacyEncrypted,
			key:    nil,
			want:   nil,
		},
		{
			name:   "legacy ciphertext with wrong key treated as empty",
			stored: legacyEncrypted,
			key:    wrongKey,
			want:   nil,
		},
		{
			name:   "garbage data treated as empty",
			stored: "not-json-and-not-ciphertext",
			key:    key,
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lb := NewDefaultLoadBalancer(nil, tt.key)
			got, err := lb.decryptConfig(tt.stored)
			if err != nil {
				t.Fatalf("decryptConfig unexpected error: %v", err)
			}
			if !stringMapEqual(got, tt.want) {
				t.Fatalf("decryptConfig = %v, want %v", got, tt.want)
			}
		})
	}
}

// stringMapEqual compares two map[string]string values; nil and empty are equal.
func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// int64SliceEqual compares two int64 slices for equality.
// Both nil and empty slices are treated as equal.
func int64SliceEqual(a, b []int64) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// SelectInstance (fail-closed behaviour)
// ---------------------------------------------------------------------------

// newSelectInstanceTestLB builds a DefaultLoadBalancer backed by sqlmock so the
// instance/usage queries made by SelectInstance can be scripted. The load
// balancer only needs the ent client; no encryption key is involved because the
// fixtures store plaintext (empty) configs.
func newSelectInstanceTestLB(t *testing.T) (*DefaultLoadBalancer, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = client.Close() })
	return NewDefaultLoadBalancer(client, nil), mock
}

// instanceRecordRows returns the sqlmock row set matching all
// payment_provider_instances columns selected by queryEnabledInstances.
func instanceRecordRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "provider_key", "name", "config", "supported_types", "enabled",
		"payment_mode", "sort_order", "limits", "refund_enabled", "allow_user_refund",
		"created_at", "updated_at",
	})
}

func addInstanceRecord(rows *sqlmock.Rows, id int64, providerKey, supportedTypes, limits string) *sqlmock.Rows {
	now := time.Now()
	return rows.AddRow(id, providerKey, "test", "", supportedTypes, true, "", 0, limits, false, false, now, now)
}

func TestSelectInstanceAllCandidatesOverLimit(t *testing.T) {
	t.Parallel()

	lb, mock := newSelectInstanceTestLB(t)
	rows := addInstanceRecord(instanceRecordRows(), 1, TypeAlipay, TypeAlipay, makeLimitsJSON(TypeAlipay, ChannelLimits{SingleMax: 10}))
	mock.ExpectQuery(`FROM "payment_provider_instances"`).WillReturnRows(rows)
	// Usage query succeeds but returns no usage; the order is still above SingleMax.
	mock.ExpectQuery(`FROM "payment_orders"`).
		WillReturnRows(sqlmock.NewRows([]string{"provider_instance_id", "sum"}))

	sel, err := lb.SelectInstance(context.Background(), "", TypeAlipay, StrategyRoundRobin, 100)
	if err == nil {
		t.Fatal("SelectInstance returned nil error, want ErrInstanceLimitsExceeded")
	}
	if !errors.Is(err, ErrInstanceLimitsExceeded) {
		t.Fatalf("SelectInstance error = %v, want it to wrap ErrInstanceLimitsExceeded", err)
	}
	if sel != nil {
		t.Fatalf("SelectInstance returned selection %+v, want nil on limits error", sel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// A malformed limits value must never make an instance selectable: the operator
// configured caps, we just cannot read them, so routing would silently exceed
// whatever those caps are.
func TestSelectInstanceExcludesUnreadableLimits(t *testing.T) {
	t.Parallel()

	lb, mock := newSelectInstanceTestLB(t)
	rows := addInstanceRecord(instanceRecordRows(), 1, TypeAlipay, TypeAlipay, "not-json{")
	mock.ExpectQuery(`FROM "payment_provider_instances"`).WillReturnRows(rows)
	mock.ExpectQuery(`FROM "payment_orders"`).
		WillReturnRows(sqlmock.NewRows([]string{"provider_instance_id", "sum"}))

	sel, err := lb.SelectInstance(context.Background(), "", TypeAlipay, StrategyRoundRobin, 50)
	if err == nil {
		t.Fatalf("SelectInstance returned selection %+v, want ErrInstanceLimitsExceeded", sel)
	}
	if !errors.Is(err, ErrInstanceLimitsExceeded) {
		t.Fatalf("SelectInstance error = %v, want it to wrap ErrInstanceLimitsExceeded", err)
	}
	if sel != nil {
		t.Fatalf("SelectInstance returned selection %+v, want nil", sel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// A readable sibling must still be usable when another instance is unreadable.
func TestSelectInstanceUsesReadableSiblingWhenOneInstanceUnreadable(t *testing.T) {
	t.Parallel()

	lb, mock := newSelectInstanceTestLB(t)
	rows := addInstanceRecord(instanceRecordRows(), 1, TypeAlipay, TypeAlipay, "not-json{")
	rows = addInstanceRecord(rows, 2, TypeAlipay, TypeAlipay, makeLimitsJSON(TypeAlipay, ChannelLimits{SingleMax: 500}))
	mock.ExpectQuery(`FROM "payment_provider_instances"`).WillReturnRows(rows)
	mock.ExpectQuery(`FROM "payment_orders"`).
		WillReturnRows(sqlmock.NewRows([]string{"provider_instance_id", "sum"}))

	sel, err := lb.SelectInstance(context.Background(), "", TypeAlipay, StrategyRoundRobin, 50)
	if err != nil {
		t.Fatalf("SelectInstance returned error %v, want the readable instance 2", err)
	}
	if sel == nil || sel.InstanceID != "2" {
		t.Fatalf("SelectInstance = %+v, want instance 2", sel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestSelectInstanceUsageQueryFailure(t *testing.T) {
	t.Parallel()

	lb, mock := newSelectInstanceTestLB(t)
	rows := addInstanceRecord(instanceRecordRows(), 1, TypeAlipay, TypeAlipay, "")
	mock.ExpectQuery(`FROM "payment_provider_instances"`).WillReturnRows(rows)
	mock.ExpectQuery(`FROM "payment_orders"`).WillReturnError(errors.New("db down"))

	sel, err := lb.SelectInstance(context.Background(), "", TypeAlipay, StrategyRoundRobin, 50)
	if err == nil {
		t.Fatal("SelectInstance returned nil error, want ErrInstanceUsageUnavailable")
	}
	if !errors.Is(err, ErrInstanceUsageUnavailable) {
		t.Fatalf("SelectInstance error = %v, want it to wrap ErrInstanceUsageUnavailable", err)
	}
	if !strings.Contains(err.Error(), "db down") {
		t.Fatalf("SelectInstance error = %v, want it to keep the underlying error text", err)
	}
	if sel != nil {
		t.Fatalf("SelectInstance returned selection %+v, want nil on usage error", sel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestSelectInstancePicksCandidateWithinLimits(t *testing.T) {
	t.Parallel()

	lb, mock := newSelectInstanceTestLB(t)
	rows := addInstanceRecord(instanceRecordRows(), 7, TypeAlipay, TypeAlipay, makeLimitsJSON(TypeAlipay, ChannelLimits{SingleMin: 1, SingleMax: 100, DailyLimit: 1000}))
	mock.ExpectQuery(`FROM "payment_provider_instances"`).WillReturnRows(rows)
	mock.ExpectQuery(`FROM "payment_orders"`).
		WillReturnRows(sqlmock.NewRows([]string{"provider_instance_id", "sum"}).AddRow("7", 20.0))

	sel, err := lb.SelectInstance(context.Background(), "", TypeAlipay, StrategyRoundRobin, 50)
	if err != nil {
		t.Fatalf("SelectInstance returned error: %v", err)
	}
	if sel == nil {
		t.Fatal("SelectInstance returned nil selection, want the in-limit instance")
	}
	if sel.InstanceID != "7" {
		t.Fatalf("SelectInstance selected instance %q, want \"7\"", sel.InstanceID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
