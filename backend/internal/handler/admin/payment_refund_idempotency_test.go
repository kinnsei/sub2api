//go:build unit

package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// newRefundIdempotencyClient builds an isolated in-memory schema for the refund
// idempotency tests. It mirrors the payment handler test setup so the ent store
// behaves the same way as in the other payment handler tests.
func newRefundIdempotencyClient(t *testing.T) *dbent.Client {
	t.Helper()
	name := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	db, err := sql.Open("sqlite", name)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// decodeRefundResponse reads the standard response envelope.
func decodeRefundEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (int, map[string]any) {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec.Code, body
}

// TestAdminRefundEndpointRequiresIdempotencyKey pins the contract the frontend
// depends on: a money-moving refund POST without an Idempotency-Key is rejected
// instead of silently executing. Without this the refund could be paid twice by a
// client retry.
func TestAdminRefundEndpointRequiresIdempotencyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Enforcement (RequireKey) is gated on ObserveOnly being false. The shipped
	// default is observe_only=true, so a keyless request is allowed through and this
	// test must opt into enforcement explicitly to pin the strict contract.
	cfg := service.DefaultIdempotencyConfig()
	cfg.ObserveOnly = false
	service.SetDefaultIdempotencyCoordinator(service.NewIdempotencyCoordinator(newMemoryIdempotencyRepoStub(), cfg))
	t.Cleanup(func() { service.SetDefaultIdempotencyCoordinator(nil) })

	client := newRefundIdempotencyClient(t)
	user, err := client.User.Create().
		SetEmail("refund-idem@example.com").
		SetPasswordHash("hash").
		SetUsername("refund-idem-user").
		Save(context.Background())
	require.NoError(t, err)
	order, err := client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(100).
		SetPayAmount(100).
		SetFeeRate(0).
		SetOutTradeNo("refund-idem-order").
		SetRechargeCode("REFUND-IDEM").
		SetPaymentTradeNo("trade-refund-idem").
		SetPaymentType(payment.TypeStripe).
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(service.OrderStatusCompleted).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com").
		Save(context.Background())
	require.NoError(t, err)

	svc := service.NewPaymentService(client, payment.NewRegistry(), nil, nil, nil, nil, nil, nil, nil)
	h := NewPaymentHandler(svc, nil)

	router := gin.New()
	router.POST("/admin/payment/orders/:id/refund", h.ProcessRefund)

	body := `{"amount":100,"reason":"no key"}`
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/payment/orders/%d/refund", order.ID), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	code, envelope := decodeRefundEnvelope(t, rec)
	require.Equal(t, http.StatusBadRequest, code, "a keyless refund must be rejected")
	require.Contains(t, fmt.Sprint(envelope), "IDEMPOTENCY_KEY_REQUIRED")

	// The order must be untouched: the request never reached the service.
	reloaded, err := client.PaymentOrder.Get(context.Background(), order.ID)
	require.NoError(t, err)
	require.Equal(t, service.OrderStatusCompleted, reloaded.Status)
}

// TestAdminRefundHandlerIdempotencyReplaysSuccessfulResult verifies the other half
// of the refund contract: the helper the ProcessRefund handler now delegates to
// executes a successful refund exactly once and replays the stored result for a
// repeated key.
//
// It drives executeAdminIdempotentJSON with the same scope/route/payload the refund
// handler uses rather than the full handler, because a real end-to-end refund needs
// a configured provider instance and gateway; the execution-once and replay
// semantics under test belong to the idempotency layer, not to the payment gateway.
func TestAdminRefundHandlerIdempotencyReplaysSuccessfulResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.SetDefaultIdempotencyCoordinator(service.NewIdempotencyCoordinator(newMemoryIdempotencyRepoStub(), service.DefaultIdempotencyConfig()))
	t.Cleanup(func() { service.SetDefaultIdempotencyCoordinator(nil) })

	// Deliberately the shipped default config (observe_only=true): deduplication must
	// work whenever a key IS supplied, independently of whether a missing key is
	// rejected. This is the case the frontend relies on.
	var executed int
	payload := map[string]any{"amount": 100, "reason": "replay", "force": false, "deduct_balance": false}

	router := gin.New()
	router.POST("/admin/payment/orders/:id/refund", func(c *gin.Context) {
		executeAdminIdempotentJSON(c, "admin.payment.refund", payload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
			executed++
			return &service.RefundResult{Success: true, BalanceDeducted: 100}, nil
		})
	})

	const key = "admin-refund-replay-key-1"
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	call := func() (*httptest.ResponseRecorder, string) {
		req := httptest.NewRequest(http.MethodPost, "/admin/payment/orders/1/refund", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec, rec.Header().Get("X-Idempotency-Replayed")
	}

	firstRec, firstReplay := call()
	require.Equal(t, http.StatusOK, firstRec.Code)
	require.Empty(t, firstReplay, "the first call is not a replay")
	require.Equal(t, 1, executed)

	secondRec, secondReplay := call()
	require.Equal(t, "true", secondReplay, "the repeated key must be served from the idempotency store")
	// Compare semantically: the replay may serialize stored JSON object keys in a
	// different order than the first response.
	var firstBody, secondBody map[string]any
	require.NoError(t, json.Unmarshal(firstRec.Body.Bytes(), &firstBody))
	require.NoError(t, json.Unmarshal(secondRec.Body.Bytes(), &secondBody))
	require.Equal(t, firstBody, secondBody, "a replay must return the stored result")
	require.Equal(t, 1, executed, "the refund must be executed exactly once for one idempotency key")
}
