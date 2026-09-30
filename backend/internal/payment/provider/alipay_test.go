//go:build unit

package provider

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/smartwalle/alipay/v3"
)

// ---------------------------------------------------------------------------
// Fixtures: a throwaway RSA key pair and a fake Alipay gateway.
//
// The gateway is a real HTTP server serving responses signed exactly the way
// Alipay signs them (RSA2/SHA-256 over the raw JSON of the business field).
// That lets the tests drive the SDK's own request, decode and signature
// verification paths instead of a stubbed function value, which is what makes
// the gateway-level failure shapes below faithful.
// ---------------------------------------------------------------------------

var (
	alipayTestKeyOnce sync.Once
	alipayTestKeyVal  *rsa.PrivateKey
)

// alipayTestKey returns a process-wide RSA key. Generating one per test would
// dominate the package's runtime.
func alipayTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	alipayTestKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate test RSA key: %v", err)
			return
		}
		alipayTestKeyVal = key
	})
	if alipayTestKeyVal == nil {
		t.Fatal("test RSA key was not usable")
	}
	return alipayTestKeyVal
}

func alipayTestPrivateKeyPEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func alipayTestPublicKeyPEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func signAlipayBytes(key *rsa.PrivateKey, data []byte) (string, error) {
	digest := sha256.Sum256(data)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

// alipayTestValuesSignature reproduces the SDK's signing input: every
// "key=value" pair of the response, sorted, joined with "&", with the sign,
// sign_type and alipay_cert_sn fields excluded.
func alipayTestValuesSignature(t *testing.T, values url.Values) string {
	t.Helper()
	pairs := make([]string, 0, len(values))
	for key, nValues := range values {
		switch key {
		case "sign", "sign_type", "alipay_cert_sn":
			continue
		}
		for _, value := range nValues {
			pairs = append(pairs, key+"="+value)
		}
	}
	sort.Strings(pairs)

	signature, err := signAlipayBytes(alipayTestKey(t), []byte(strings.Join(pairs, "&")))
	if err != nil {
		t.Fatalf("sign values: %v", err)
	}
	return signature
}

// newAlipayTestClient builds a real SDK client pointed at handler.
func newAlipayTestClient(t *testing.T, handler http.HandlerFunc) *alipay.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := alipay.New(
		"2021000000000000",
		alipayTestPrivateKeyPEM(t, alipayTestKey(t)),
		true,
		alipay.WithHTTPClient(server.Client()),
		alipay.WithProductionGateway(server.URL),
	)
	if err != nil {
		t.Fatalf("alipay.New: %v", err)
	}
	if err := client.LoadAliPayPublicKey(alipayTestPublicKeyPEM(t, alipayTestKey(t))); err != nil {
		t.Fatalf("LoadAliPayPublicKey: %v", err)
	}
	return client
}

func alipayTestProvider(t *testing.T, handler http.HandlerFunc) *Alipay {
	t.Helper()
	return &Alipay{
		client: newAlipayTestClient(t, handler),
		config: map[string]string{"appId": "2021000000000000"},
	}
}

// writeAlipaySignedResponse writes a signed success-shaped response body.
func writeAlipaySignedResponse(t *testing.T, w http.ResponseWriter, method string, payload any) {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	signature, err := signAlipayBytes(alipayTestKey(t), raw)
	if err != nil {
		t.Fatalf("sign payload: %v", err)
	}

	field := strings.ReplaceAll(method, ".", "_") + "_response"
	body := "{" +
		strconv.Quote(field) + ":" + string(raw) + "," +
		`"sign":` + strconv.Quote(signature) + "," +
		`"sign_type":"RSA2"}`
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write response: %v", err)
	}
}

// writeAlipayErrorResponse writes the unsigned error body the gateway returns
// for request-level failures such as an unknown out_trade_no.
func writeAlipayErrorResponse(w http.ResponseWriter, code, msg, subCode, subMsg string) {
	payload, _ := json.Marshal(map[string]string{
		"code":     code,
		"msg":      msg,
		"sub_code": subCode,
		"sub_msg":  subMsg,
	})
	_, _ = w.Write([]byte(`{"error_response":` + string(payload) + `}`))
}

func writeAlipayBadSignatureResponse(t *testing.T, w http.ResponseWriter, method string) {
	t.Helper()
	field := strings.ReplaceAll(method, ".", "_") + "_response"
	body := "{" +
		strconv.Quote(field) + `:{"code":"10000","msg":"Success"},` +
		`"sign":"bm90LWEtcmVhbC1zaWduYXR1cmU=","sign_type":"RSA2"}`
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write response: %v", err)
	}
}

// tradeNotExistError is the shape the SDK really produces for a gateway
// rejection: Client.decode returns a *alipay.Error whose Error() renders only
// "<code> - <sub_msg>" and therefore never contains the sub_code.
func tradeNotExistError() *alipay.Error {
	return &alipay.Error{
		Code:    alipay.CodeBusinessFailed,
		Msg:     "Business Failed",
		SubCode: alipayErrTradeNotExist,
		SubMsg:  "交易不存在",
	}
}

func systemError() *alipay.Error {
	return &alipay.Error{
		Code:    alipay.CodeBusinessFailed,
		Msg:     "Business Failed",
		SubCode: "ACQ.SYSTEM_ERROR",
		SubMsg:  "系统错误",
	}
}

// ---------------------------------------------------------------------------
// isTradeNotExist
// ---------------------------------------------------------------------------

func TestIsTradeNotExist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error returns false",
			err:  nil,
			want: false,
		},
		{
			name: "sdk error with ACQ.TRADE_NOT_EXIST sub code returns true",
			err:  tradeNotExistError(),
			want: true,
		},
		{
			// The provider wraps every SDK error with fmt.Errorf("...: %w", err),
			// and the interface value carries a *alipay.Error.
			name: "wrapped sdk error still returns true",
			err:  fmt.Errorf("alipay TradeQuery: %w", error(tradeNotExistError())),
			want: true,
		},
		{
			name: "sdk error with another sub code returns false",
			err:  systemError(),
			want: false,
		},
		{
			// alipay.Error.Error() renders "<code> - <sub_msg>" and never the
			// sub_code, so a plain error that merely mentions the code in its
			// text is not a gateway rejection.
			name: "plain error whose text mentions the code returns false",
			err:  errors.New("alipay: sub_code=ACQ.TRADE_NOT_EXIST, sub_msg=交易不存在"),
			want: false,
		},
		{
			name: "plain error equal to the constant returns false",
			err:  errors.New(alipayErrTradeNotExist),
			want: false,
		},
		{
			name: "sdk sentinel error returns false",
			err:  alipay.ErrBadResponse,
			want: false,
		},
		{
			name: "sdk error with an empty sub code returns false",
			err:  &alipay.Error{Code: alipay.CodeBusinessFailed, Msg: "Business Failed"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isTradeNotExist(tt.err)
			if got != tt.want {
				t.Errorf("isTradeNotExist(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// getClient
// ---------------------------------------------------------------------------

// The SDK defaults Client to http.DefaultClient, which has no timeout, so a
// hung gateway would block a request goroutine forever.
func TestAlipayGetClientSetsHTTPTimeout(t *testing.T) {
	t.Parallel()

	key := alipayTestKey(t)
	provider, err := NewAlipay("test-instance", map[string]string{
		"appId":      "2021000000000000",
		"privateKey": alipayTestPrivateKeyPEM(t, key),
		"publicKey":  alipayTestPublicKeyPEM(t, key),
	})
	if err != nil {
		t.Fatalf("NewAlipay: %v", err)
	}

	client, err := provider.getClient()
	if err != nil {
		t.Fatalf("getClient: %v", err)
	}
	if client.Client == nil {
		t.Fatal("client.Client is nil")
	}
	if client.Client.Timeout != alipayHTTPTimeout {
		t.Fatalf("client timeout = %v, want %v", client.Client.Timeout, alipayHTTPTimeout)
	}
	if client.Client.Timeout <= 0 {
		t.Fatal("client timeout must be positive; an unset timeout means requests can never time out")
	}
}

// ---------------------------------------------------------------------------
// QueryOrder
// ---------------------------------------------------------------------------

// A query for a trade Alipay never created is answered with an error_response
// carrying sub_code ACQ.TRADE_NOT_EXIST. That is "not paid yet", not a hard
// failure, and the caller dereferences the response unconditionally.
func TestAlipayQueryOrderTreatsUnknownTradeAsPending(t *testing.T) {
	provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
			return
		}
		if got := r.PostForm.Get("method"); got != "alipay.trade.query" {
			t.Errorf("method = %q, want %q", got, "alipay.trade.query")
		}
		writeAlipayErrorResponse(w, string(alipay.CodeBusinessFailed), "Business Failed", alipayErrTradeNotExist, "交易不存在")
	})

	resp, err := provider.QueryOrder(context.Background(), "sub2_unknown")
	if err != nil {
		t.Fatalf("QueryOrder for an unknown trade must not fail: %v", err)
	}
	if resp == nil {
		t.Fatal("response is nil; the caller dereferences it")
	}
	if resp.Status != payment.ProviderStatusPending {
		t.Fatalf("status = %q, want %q", resp.Status, payment.ProviderStatusPending)
	}
	if resp.TradeNo != "sub2_unknown" {
		t.Fatalf("TradeNo = %q, want %q", resp.TradeNo, "sub2_unknown")
	}
	if resp.Amount != 0 {
		t.Fatalf("Amount = %v, want 0 for an unpaid trade", resp.Amount)
	}
	if resp.Metadata["app_id"] != "2021000000000000" {
		t.Fatalf("metadata = %+v, want app_id", resp.Metadata)
	}
}

// Any other gateway sub_code must still surface as an error.
func TestAlipayQueryOrderPropagatesOtherGatewayErrors(t *testing.T) {
	provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
			return
		}
		writeAlipayErrorResponse(w, string(alipay.CodeBusinessFailed), "Business Failed", "ACQ.SYSTEM_ERROR", "系统错误")
	})

	resp, err := provider.QueryOrder(context.Background(), "sub2_broken")
	if err == nil {
		t.Fatal("expected an error for a non trade-not-exist gateway failure")
	}
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
}

// Alipay can also answer code 10000 with an empty body (no trade_status and no
// amount). That must degrade to pending instead of a parse error.
func TestAlipayQueryOrderTreatsEmptyBodyAsPending(t *testing.T) {
	provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
			return
		}
		writeAlipaySignedResponse(t, w, r.PostForm.Get("method"), &alipay.TradeQueryRsp{
			Error: alipay.Error{Code: alipay.CodeSuccess, Msg: "Success"},
		})
	})

	resp, err := provider.QueryOrder(context.Background(), "sub2_empty")
	if err != nil {
		t.Fatalf("an empty trade body must not be a hard error: %v", err)
	}
	if resp == nil {
		t.Fatal("response is nil; the caller dereferences it")
	}
	if resp.Status != payment.ProviderStatusPending {
		t.Fatalf("status = %q, want %q", resp.Status, payment.ProviderStatusPending)
	}
}

func TestAlipayQueryOrderMapsTradeStatuses(t *testing.T) {
	tests := []struct {
		name       string
		rsp        *alipay.TradeQueryRsp
		wantStatus string
		wantAmount float64
		wantTrade  string
		wantPaidAt string
	}{
		{
			name: "successful trade maps to paid",
			rsp: &alipay.TradeQueryRsp{
				Error:       alipay.Error{Code: alipay.CodeSuccess},
				TradeNo:     "2021000000000001",
				TradeStatus: alipay.TradeStatusSuccess,
				TotalAmount: "88.00",
				SendPayDate: "2024-05-01 12:00:00",
			},
			wantStatus: payment.ProviderStatusPaid,
			wantAmount: 88,
			wantTrade:  "2021000000000001",
			wantPaidAt: "2024-05-01 12:00:00",
		},
		{
			name: "finished trade maps to paid",
			rsp: &alipay.TradeQueryRsp{
				Error:       alipay.Error{Code: alipay.CodeSuccess},
				TradeNo:     "2021000000000002",
				TradeStatus: alipay.TradeStatusFinished,
				TotalAmount: "12.30",
			},
			wantStatus: payment.ProviderStatusPaid,
			wantAmount: 12.3,
			wantTrade:  "2021000000000002",
		},
		{
			name: "closed trade maps to failed",
			rsp: &alipay.TradeQueryRsp{
				Error:       alipay.Error{Code: alipay.CodeSuccess},
				TradeNo:     "2021000000000003",
				TradeStatus: alipay.TradeStatusClosed,
				TotalAmount: "9.99",
			},
			wantStatus: payment.ProviderStatusFailed,
			wantAmount: 9.99,
			wantTrade:  "2021000000000003",
		},
		{
			name: "waiting trade maps to pending",
			rsp: &alipay.TradeQueryRsp{
				Error:       alipay.Error{Code: alipay.CodeSuccess},
				TradeNo:     "2021000000000004",
				TradeStatus: alipay.TradeStatusWaitBuyerPay,
				TotalAmount: "5.00",
			},
			wantStatus: payment.ProviderStatusPending,
			wantAmount: 5,
			wantTrade:  "2021000000000004",
		},
		{
			name: "amount falls back to receipt amount and is trimmed",
			rsp: &alipay.TradeQueryRsp{
				Error:         alipay.Error{Code: alipay.CodeSuccess},
				TradeNo:       "2021000000000005",
				TradeStatus:   alipay.TradeStatusSuccess,
				TotalAmount:   "",
				ReceiptAmount: " 66.60 ",
			},
			wantStatus: payment.ProviderStatusPaid,
			wantAmount: 66.6,
			wantTrade:  "2021000000000005",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("ParseForm: %v", err)
					return
				}
				writeAlipaySignedResponse(t, w, r.PostForm.Get("method"), tt.rsp)
			})

			resp, err := provider.QueryOrder(context.Background(), "sub2_query")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp == nil {
				t.Fatal("response is nil")
			}
			if resp.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", resp.Status, tt.wantStatus)
			}
			if resp.Amount != tt.wantAmount {
				t.Fatalf("amount = %v, want %v", resp.Amount, tt.wantAmount)
			}
			if resp.TradeNo != tt.wantTrade {
				t.Fatalf("TradeNo = %q, want %q", resp.TradeNo, tt.wantTrade)
			}
			if resp.PaidAt != tt.wantPaidAt {
				t.Fatalf("PaidAt = %q, want %q", resp.PaidAt, tt.wantPaidAt)
			}
		})
	}
}

// A trade that carries a real status but no usable amount is a genuine data
// problem and must not be downgraded to pending.
func TestAlipayQueryOrderRejectsUnparseableAmountForKnownStatus(t *testing.T) {
	provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
			return
		}
		writeAlipaySignedResponse(t, w, r.PostForm.Get("method"), &alipay.TradeQueryRsp{
			Error:       alipay.Error{Code: alipay.CodeSuccess},
			TradeNo:     "2021000000000006",
			TradeStatus: alipay.TradeStatusSuccess,
			TotalAmount: "not-a-number",
		})
	})

	resp, err := provider.QueryOrder(context.Background(), "sub2_bad_amount")
	if err == nil {
		t.Fatal("expected an error for a paid trade without a parseable amount")
	}
	if !strings.Contains(err.Error(), "parse amount") {
		t.Fatalf("error = %v, want a parse-amount error", err)
	}
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
}

// A response that cannot be verified is an error, never a silent pending.
func TestAlipayQueryOrderRejectsBadSignature(t *testing.T) {
	provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
			return
		}
		writeAlipayBadSignatureResponse(t, w, r.PostForm.Get("method"))
	})

	resp, err := provider.QueryOrder(context.Background(), "sub2_bad_sign")
	if err == nil {
		t.Fatal("expected a signature verification error")
	}
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
}

// ---------------------------------------------------------------------------
// VerifyNotification
// ---------------------------------------------------------------------------

func signedNotificationBody(t *testing.T, fields map[string]string) string {
	t.Helper()

	values := url.Values{}
	for key, value := range fields {
		values.Set(key, value)
	}
	values.Set("sign", alipayTestValuesSignature(t, values))
	values.Set("sign_type", "RSA2")
	return values.Encode()
}

func TestAlipayVerifyNotification(t *testing.T) {
	provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("VerifyNotification must not call the gateway, got %s", r.URL.Path)
	})

	rawBody := signedNotificationBody(t, map[string]string{
		"app_id":       "2021000000000000",
		"trade_no":     "2021000000000010",
		"out_trade_no": "sub2_notify",
		"trade_status": string(alipay.TradeStatusSuccess),
		"total_amount": " 88.00 ",
		"gmt_payment":  "2024-05-01 12:00:00",
	})

	notification, err := provider.VerifyNotification(context.Background(), rawBody, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if notification == nil {
		t.Fatal("notification is nil")
	}
	if notification.Status != payment.ProviderStatusSuccess {
		t.Fatalf("status = %q, want %q", notification.Status, payment.ProviderStatusSuccess)
	}
	if notification.TradeNo != "2021000000000010" {
		t.Fatalf("TradeNo = %q", notification.TradeNo)
	}
	if notification.OrderID != "sub2_notify" {
		t.Fatalf("OrderID = %q", notification.OrderID)
	}
	if notification.Amount != 88 {
		t.Fatalf("amount = %v, want 88", notification.Amount)
	}
	if notification.RawData != rawBody {
		t.Fatalf("RawData = %q, want the original body", notification.RawData)
	}
	if notification.Metadata["app_id"] != "2021000000000000" {
		t.Fatalf("metadata = %+v, want app_id", notification.Metadata)
	}
}

func TestAlipayVerifyNotificationMapsNonSuccessStatusToFailed(t *testing.T) {
	provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("VerifyNotification must not call the gateway, got %s", r.URL.Path)
	})

	rawBody := signedNotificationBody(t, map[string]string{
		"app_id":       "2021000000000000",
		"trade_no":     "2021000000000011",
		"out_trade_no": "sub2_notify_wait",
		"trade_status": string(alipay.TradeStatusWaitBuyerPay),
		"total_amount": "18.00",
	})

	notification, err := provider.VerifyNotification(context.Background(), rawBody, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if notification.Status != payment.ProviderStatusFailed {
		t.Fatalf("status = %q, want %q", notification.Status, payment.ProviderStatusFailed)
	}
}

func TestAlipayVerifyNotificationRejectsTamperedBody(t *testing.T) {
	provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("VerifyNotification must not call the gateway, got %s", r.URL.Path)
	})

	rawBody := signedNotificationBody(t, map[string]string{
		"app_id":       "2021000000000000",
		"trade_no":     "2021000000000012",
		"out_trade_no": "sub2_notify_tampered",
		"trade_status": string(alipay.TradeStatusSuccess),
		"total_amount": "88.00",
	})
	// Re-point the payment at a different order: the signature no longer covers
	// the body, so this must be rejected rather than credited.
	tampered := strings.Replace(rawBody, "sub2_notify_tampered", "sub2_notify_attacker", 1)

	if _, err := provider.VerifyNotification(context.Background(), tampered, nil); err == nil {
		t.Fatal("expected a signature verification error for a tampered notification")
	}
}

func TestAlipayVerifyNotificationRejectsUnparseableAmount(t *testing.T) {
	provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("VerifyNotification must not call the gateway, got %s", r.URL.Path)
	})

	rawBody := signedNotificationBody(t, map[string]string{
		"app_id":       "2021000000000000",
		"trade_no":     "2021000000000013",
		"out_trade_no": "sub2_notify_bad_amount",
		"trade_status": string(alipay.TradeStatusSuccess),
		"total_amount": "not-a-number",
	})

	if _, err := provider.VerifyNotification(context.Background(), rawBody, nil); err == nil {
		t.Fatal("expected an error for an unparseable notification amount")
	}
}

// ---------------------------------------------------------------------------
// Refund
// ---------------------------------------------------------------------------

func TestAlipayRefundFailsOnBusinessError(t *testing.T) {
	origRefund := alipayTradeRefund
	t.Cleanup(func() { alipayTradeRefund = origRefund })

	alipayTradeRefund = func(_ context.Context, _ *alipay.Client, _ alipay.TradeRefund) (*alipay.TradeRefundRsp, error) {
		// A business rejection arrives with a nil Go error and a failure code.
		return &alipay.TradeRefundRsp{
			Error: *tradeNotExistError(),
		}, nil
	}

	provider := &Alipay{client: &alipay.Client{}}
	resp, err := provider.Refund(context.Background(), payment.RefundRequest{
		OrderID: "sub2_refund_failed",
		Amount:  "12.34",
		Reason:  "user request",
	})
	if err == nil {
		t.Fatal("expected an error when the gateway rejects the refund")
	}
	if !strings.Contains(err.Error(), alipayErrTradeNotExist) && !strings.Contains(err.Error(), "TradeRefund failed") {
		t.Fatalf("error = %v, want a TradeRefund business failure", err)
	}
	if resp != nil {
		t.Fatalf("response = %+v, want nil so the caller does not record a refund", resp)
	}
}

func TestAlipayRefundRejectsEmptyResponse(t *testing.T) {
	origRefund := alipayTradeRefund
	t.Cleanup(func() { alipayTradeRefund = origRefund })

	alipayTradeRefund = func(_ context.Context, _ *alipay.Client, _ alipay.TradeRefund) (*alipay.TradeRefundRsp, error) {
		return nil, nil
	}

	provider := &Alipay{client: &alipay.Client{}}
	if _, err := provider.Refund(context.Background(), payment.RefundRequest{OrderID: "sub2_nil_rsp", Amount: "1.00"}); err == nil {
		t.Fatal("expected an error for a nil TradeRefund response")
	}
}

// An explicit RefundNo is the value the caller persisted, so it must win over
// the derived default and be reported back as-is.
func TestAlipayRefundPrefersExplicitRefundNo(t *testing.T) {
	origRefund := alipayTradeRefund
	t.Cleanup(func() { alipayTradeRefund = origRefund })

	var gotParam alipay.TradeRefund
	alipayTradeRefund = func(_ context.Context, _ *alipay.Client, param alipay.TradeRefund) (*alipay.TradeRefundRsp, error) {
		gotParam = param
		return &alipay.TradeRefundRsp{
			Error:      alipay.Error{Code: alipay.CodeSuccess},
			FundChange: alipayFundChangeYes,
		}, nil
	}

	provider := &Alipay{client: &alipay.Client{}}
	resp, err := provider.Refund(context.Background(), payment.RefundRequest{
		OrderID:  "sub2_explicit",
		Amount:   "12.34",
		RefundNo: "  refund-persisted-by-caller  ",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotParam.OutRequestNo != "refund-persisted-by-caller" {
		t.Fatalf("OutRequestNo = %q, want the trimmed explicit RefundNo", gotParam.OutRequestNo)
	}
	if resp.RefundID != "refund-persisted-by-caller" {
		t.Fatalf("RefundID = %q, want the explicit RefundNo", resp.RefundID)
	}
	if resp.Status != payment.ProviderStatusSuccess {
		t.Fatalf("status = %q, want %q", resp.Status, payment.ProviderStatusSuccess)
	}
}

// ---------------------------------------------------------------------------
// QueryRefund
// ---------------------------------------------------------------------------

func TestAlipayQueryRefundFailsOnBusinessError(t *testing.T) {
	origQuery := alipayTradeFastPayRefundQuery
	t.Cleanup(func() { alipayTradeFastPayRefundQuery = origQuery })

	alipayTradeFastPayRefundQuery = func(_ context.Context, _ *alipay.Client, _ alipay.TradeFastPayRefundQuery) (*alipay.TradeFastPayRefundQueryRsp, error) {
		return &alipay.TradeFastPayRefundQueryRsp{
			Error: *systemError(),
		}, nil
	}

	provider := &Alipay{client: &alipay.Client{}}
	resp, err := provider.QueryRefund(context.Background(), payment.RefundQueryRequest{
		OrderID:  "sub2_q_failed",
		RefundID: "sub2_q_failed-refund-1234",
	})
	if err == nil {
		t.Fatal("expected an error when the gateway rejects the refund query")
	}
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
}

func TestAlipayQueryRefundRejectsEmptyResponse(t *testing.T) {
	origQuery := alipayTradeFastPayRefundQuery
	t.Cleanup(func() { alipayTradeFastPayRefundQuery = origQuery })

	alipayTradeFastPayRefundQuery = func(_ context.Context, _ *alipay.Client, _ alipay.TradeFastPayRefundQuery) (*alipay.TradeFastPayRefundQueryRsp, error) {
		return nil, nil
	}

	provider := &Alipay{client: &alipay.Client{}}
	if _, err := provider.QueryRefund(context.Background(), payment.RefundQueryRequest{
		OrderID:  "sub2_q_nil",
		RefundID: "sub2_q_nil-refund-1234",
	}); err == nil {
		t.Fatal("expected an error for a nil refund query response")
	}
}

// An explicit RefundID must win over the derived request number.
func TestAlipayQueryRefundPrefersExplicitRefundID(t *testing.T) {
	origQuery := alipayTradeFastPayRefundQuery
	t.Cleanup(func() { alipayTradeFastPayRefundQuery = origQuery })

	var gotParam alipay.TradeFastPayRefundQuery
	alipayTradeFastPayRefundQuery = func(_ context.Context, _ *alipay.Client, param alipay.TradeFastPayRefundQuery) (*alipay.TradeFastPayRefundQueryRsp, error) {
		gotParam = param
		return &alipay.TradeFastPayRefundQueryRsp{
			Error:        alipay.Error{Code: alipay.CodeSuccess},
			RefundStatus: alipayRefundStatusSuccess,
		}, nil
	}

	provider := &Alipay{client: &alipay.Client{}}
	resp, err := provider.QueryRefund(context.Background(), payment.RefundQueryRequest{
		OrderID:  "sub2_q_explicit",
		RefundID: "  explicit-refund-id  ",
		Amount:   "12.34",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotParam.OutRequestNo != "explicit-refund-id" {
		t.Fatalf("OutRequestNo = %q, want the trimmed explicit RefundID", gotParam.OutRequestNo)
	}
	if resp.RefundID != "explicit-refund-id" {
		t.Fatalf("RefundID = %q, want the explicit RefundID", resp.RefundID)
	}
}

// ---------------------------------------------------------------------------
// CancelPayment
// ---------------------------------------------------------------------------

func TestAlipayCancelPayment(t *testing.T) {
	tests := []struct {
		name      string
		respond   func(t *testing.T, w http.ResponseWriter, method string)
		wantErr   bool
		errSubstr string
	}{
		{
			name: "successful close returns nil",
			respond: func(t *testing.T, w http.ResponseWriter, method string) {
				writeAlipaySignedResponse(t, w, method, &alipay.TradeCloseRsp{
					Error:      alipay.Error{Code: alipay.CodeSuccess},
					OutTradeNo: "sub2_cancel",
				})
			},
		},
		{
			name: "already closed trade is treated as success",
			respond: func(t *testing.T, w http.ResponseWriter, method string) {
				writeAlipaySignedResponse(t, w, method, &alipay.TradeCloseRsp{
					Error: *tradeNotExistError(),
				})
			},
		},
		{
			name: "gateway-level trade not exist is treated as success",
			respond: func(t *testing.T, w http.ResponseWriter, _ string) {
				writeAlipayErrorResponse(w, string(alipay.CodeBusinessFailed), "Business Failed", alipayErrTradeNotExist, "交易不存在")
			},
		},
		{
			name: "rejected close surfaces as an error",
			respond: func(t *testing.T, w http.ResponseWriter, method string) {
				writeAlipaySignedResponse(t, w, method, &alipay.TradeCloseRsp{
					Error: *systemError(),
				})
			},
			wantErr:   true,
			errSubstr: "TradeClose failed",
		},
		{
			name: "gateway-level system error surfaces as an error",
			respond: func(t *testing.T, w http.ResponseWriter, _ string) {
				writeAlipayErrorResponse(w, string(alipay.CodeBusinessFailed), "Business Failed", "ACQ.SYSTEM_ERROR", "系统错误")
			},
			wantErr:   true,
			errSubstr: "TradeClose",
		},
		{
			name: "unverifiable response surfaces as an error",
			respond: func(t *testing.T, w http.ResponseWriter, method string) {
				writeAlipayBadSignatureResponse(t, w, method)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotOutTradeNo string
			provider := alipayTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("ParseForm: %v", err)
					return
				}
				method := r.PostForm.Get("method")
				if method != "alipay.trade.close" {
					t.Errorf("method = %q, want %q", method, "alipay.trade.close")
				}
				var biz struct {
					OutTradeNo string `json:"out_trade_no"`
				}
				if err := json.Unmarshal([]byte(r.PostForm.Get("biz_content")), &biz); err != nil {
					t.Errorf("unmarshal biz_content: %v", err)
				}
				gotOutTradeNo = biz.OutTradeNo
				tt.respond(t, w, method)
			})

			err := provider.CancelPayment(context.Background(), "sub2_cancel")
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.errSubstr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotOutTradeNo != "sub2_cancel" {
				t.Fatalf("out_trade_no = %q, want %q", gotOutTradeNo, "sub2_cancel")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NewAlipay
// ---------------------------------------------------------------------------

func TestNewAlipay(t *testing.T) {
	t.Parallel()

	validConfig := map[string]string{
		"appId":      "2021001234567890",
		"privateKey": "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC...",
	}

	// helper to clone and override config fields
	withOverride := func(overrides map[string]string) map[string]string {
		cfg := make(map[string]string, len(validConfig))
		for k, v := range validConfig {
			cfg[k] = v
		}
		for k, v := range overrides {
			cfg[k] = v
		}
		return cfg
	}

	tests := []struct {
		name      string
		config    map[string]string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "valid config succeeds",
			config:  validConfig,
			wantErr: false,
		},
		{
			name:      "missing appId",
			config:    withOverride(map[string]string{"appId": ""}),
			wantErr:   true,
			errSubstr: "appId",
		},
		{
			name:      "missing privateKey",
			config:    withOverride(map[string]string{"privateKey": ""}),
			wantErr:   true,
			errSubstr: "privateKey",
		},
		{
			name:      "nil config map returns error for appId",
			config:    map[string]string{},
			wantErr:   true,
			errSubstr: "appId",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewAlipay("test-instance", tt.config)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("expected non-nil Alipay instance")
			}
			if got.instanceID != "test-instance" {
				t.Errorf("instanceID = %q, want %q", got.instanceID, "test-instance")
			}
		})
	}
}

func TestCreateTradeUsesPagePayForDesktop(t *testing.T) {
	origPreCreate := alipayTradePreCreate
	origPagePay := alipayTradePagePay
	origWapPay := alipayTradeWapPay
	t.Cleanup(func() {
		alipayTradePreCreate = origPreCreate
		alipayTradePagePay = origPagePay
		alipayTradeWapPay = origWapPay
	})

	preCreateCalls := 0
	pagePayCalls := 0
	wapPayCalls := 0
	alipayTradePreCreate = func(ctx context.Context, client *alipay.Client, param alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error) {
		preCreateCalls++
		return nil, errors.New("merchant does not have FACE_TO_FACE_PAYMENT")
	}
	alipayTradePagePay = func(client *alipay.Client, param alipay.TradePagePay) (*url.URL, error) {
		pagePayCalls++
		if param.OutTradeNo != "sub2_100" {
			t.Fatalf("out_trade_no = %q, want %q", param.OutTradeNo, "sub2_100")
		}
		if param.NotifyURL != "https://merchant.example.com/api/v1/payment/webhook/alipay" {
			t.Fatalf("notify_url = %q", param.NotifyURL)
		}
		return url.Parse("https://openapi.alipay.com/gateway.do?page-pay")
	}
	alipayTradeWapPay = func(client *alipay.Client, param alipay.TradeWapPay) (*url.URL, error) {
		wapPayCalls++
		return url.Parse("https://openapi.alipay.com/gateway.do?wap-pay")
	}

	provider := &Alipay{}
	resp, err := provider.createDesktopTrade(context.Background(), &alipay.Client{}, payment.CreatePaymentRequest{
		OrderID: "sub2_100",
		Amount:  "88.00",
		Subject: "Balance recharge",
	}, "https://merchant.example.com/api/v1/payment/webhook/alipay", "https://merchant.example.com/payment/result")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preCreateCalls != 1 {
		t.Fatalf("precreate calls = %d, want 1", preCreateCalls)
	}
	if pagePayCalls != 1 {
		t.Fatalf("page pay calls = %d, want 1", pagePayCalls)
	}
	if wapPayCalls != 0 {
		t.Fatalf("wap pay calls = %d, want 0", wapPayCalls)
	}
	if resp.PayURL == "" {
		t.Fatal("expected pay_url for desktop page pay")
	}
	// page.pay returns a checkout page URL, not a scannable QR payload —
	// it must never be exposed via QRCode (the frontend would render an
	// unscannable image from it).
	if resp.QRCode != "" {
		t.Fatalf("qr_code = %q, want empty for page pay", resp.QRCode)
	}
}

// When the provider instance is configured with paymentMode == "redirect",
// the desktop flow must skip precreate and go straight to page.pay.
func TestCreateTradeRedirectModeSkipsPrecreate(t *testing.T) {
	origPreCreate := alipayTradePreCreate
	origPagePay := alipayTradePagePay
	t.Cleanup(func() {
		alipayTradePreCreate = origPreCreate
		alipayTradePagePay = origPagePay
	})

	preCreateCalls := 0
	pagePayCalls := 0
	alipayTradePreCreate = func(ctx context.Context, client *alipay.Client, param alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error) {
		preCreateCalls++
		return &alipay.TradePreCreateRsp{
			Error:  alipay.Error{Code: alipay.CodeSuccess},
			QRCode: "https://qr.alipay.example.com/precreate-token",
		}, nil
	}
	alipayTradePagePay = func(client *alipay.Client, param alipay.TradePagePay) (*url.URL, error) {
		pagePayCalls++
		if param.ProductCode != alipayProductCodePagePay {
			t.Fatalf("product_code = %q, want %q", param.ProductCode, alipayProductCodePagePay)
		}
		return url.Parse("https://openapi.alipay.com/gateway.do?page-pay")
	}

	provider := &Alipay{
		config: map[string]string{"paymentMode": "redirect"},
	}
	resp, err := provider.createDesktopTrade(context.Background(), &alipay.Client{}, payment.CreatePaymentRequest{
		OrderID: "sub2_103",
		Amount:  "12.00",
		Subject: "Balance recharge",
	}, "https://merchant.example.com/api/v1/payment/webhook/alipay", "https://merchant.example.com/payment/result")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preCreateCalls != 0 {
		t.Fatalf("precreate calls = %d, want 0 (redirect mode must skip precreate)", preCreateCalls)
	}
	if pagePayCalls != 1 {
		t.Fatalf("page pay calls = %d, want 1", pagePayCalls)
	}
	if resp.PayURL == "" {
		t.Fatal("expected pay_url for redirect mode")
	}
	if resp.QRCode != "" {
		t.Fatalf("qr_code = %q, want empty for redirect mode", resp.QRCode)
	}
}

func TestCreateTradeUsesWapPayForMobile(t *testing.T) {
	origWapPay := alipayTradeWapPay
	t.Cleanup(func() {
		alipayTradeWapPay = origWapPay
	})

	wapPayCalls := 0
	alipayTradeWapPay = func(client *alipay.Client, param alipay.TradeWapPay) (*url.URL, error) {
		wapPayCalls++
		if param.ReturnURL != "https://merchant.example.com/payment/result" {
			t.Fatalf("return_url = %q", param.ReturnURL)
		}
		return url.Parse("https://openapi.alipay.com/gateway.do?wap-pay")
	}

	provider := &Alipay{}
	resp, err := provider.createWapTrade(&alipay.Client{}, payment.CreatePaymentRequest{
		OrderID:  "sub2_101",
		Amount:   "18.00",
		Subject:  "Balance recharge",
		IsMobile: true,
	}, "https://merchant.example.com/api/v1/payment/webhook/alipay", "https://merchant.example.com/payment/result")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wapPayCalls != 1 {
		t.Fatalf("wap pay calls = %d, want 1", wapPayCalls)
	}
	if resp.PayURL == "" {
		t.Fatal("expected pay_url for mobile wap pay")
	}
}

func TestCreatePaymentUsesPrecreateForMobileWhenEnabled(t *testing.T) {
	origPreCreate := alipayTradePreCreate
	origWapPay := alipayTradeWapPay
	t.Cleanup(func() {
		alipayTradePreCreate = origPreCreate
		alipayTradeWapPay = origWapPay
	})

	precreateCalls := 0
	wapPayCalls := 0
	alipayTradePreCreate = func(_ context.Context, _ *alipay.Client, param alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error) {
		precreateCalls++
		if param.OutTradeNo != "sub2_mobile_precreate" {
			t.Fatalf("out_trade_no = %q", param.OutTradeNo)
		}
		if param.ProductCode != alipayProductCodePreCreate {
			t.Fatalf("product_code = %q, want %q", param.ProductCode, alipayProductCodePreCreate)
		}
		return &alipay.TradePreCreateRsp{
			Error:  alipay.Error{Code: alipay.CodeSuccess},
			QRCode: "https://qr.alipay.example.com/mobile-dynamic-token",
		}, nil
	}
	alipayTradeWapPay = func(_ *alipay.Client, _ alipay.TradeWapPay) (*url.URL, error) {
		wapPayCalls++
		return url.Parse("https://openapi.alipay.com/gateway.do?wap-pay")
	}

	provider := &Alipay{client: &alipay.Client{}, config: map[string]string{}}
	resp, err := provider.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		OrderID:               "sub2_mobile_precreate",
		Amount:                "28.00",
		Subject:               "Balance recharge",
		IsMobile:              true,
		AlipayMobilePrecreate: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if precreateCalls != 1 || wapPayCalls != 0 {
		t.Fatalf("precreate calls = %d, wap calls = %d; want 1, 0", precreateCalls, wapPayCalls)
	}
	if resp.QRCode != "https://qr.alipay.example.com/mobile-dynamic-token" || resp.PayURL != "" {
		t.Fatalf("unexpected response: qr_code=%q pay_url=%q", resp.QRCode, resp.PayURL)
	}
}

func TestCreatePaymentKeepsWapPayForMobileWhenPrecreateDisabled(t *testing.T) {
	origPreCreate := alipayTradePreCreate
	origWapPay := alipayTradeWapPay
	t.Cleanup(func() {
		alipayTradePreCreate = origPreCreate
		alipayTradeWapPay = origWapPay
	})

	precreateCalls := 0
	wapPayCalls := 0
	alipayTradePreCreate = func(_ context.Context, _ *alipay.Client, _ alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error) {
		precreateCalls++
		return nil, errors.New("unexpected precreate call")
	}
	alipayTradeWapPay = func(_ *alipay.Client, _ alipay.TradeWapPay) (*url.URL, error) {
		wapPayCalls++
		return url.Parse("https://openapi.alipay.com/gateway.do?wap-pay")
	}

	provider := &Alipay{client: &alipay.Client{}, config: map[string]string{}}
	resp, err := provider.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		OrderID:  "sub2_mobile_wap",
		Amount:   "18.00",
		Subject:  "Balance recharge",
		IsMobile: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if precreateCalls != 0 || wapPayCalls != 1 {
		t.Fatalf("precreate calls = %d, wap calls = %d; want 0, 1", precreateCalls, wapPayCalls)
	}
	if resp.PayURL == "" || resp.QRCode != "" {
		t.Fatalf("unexpected response: qr_code=%q pay_url=%q", resp.QRCode, resp.PayURL)
	}
}

func TestCreateTradeUsesPrecreateForDesktopWhenAvailable(t *testing.T) {
	origPreCreate := alipayTradePreCreate
	origPagePay := alipayTradePagePay
	t.Cleanup(func() {
		alipayTradePreCreate = origPreCreate
		alipayTradePagePay = origPagePay
	})

	preCreateCalls := 0
	pagePayCalls := 0
	alipayTradePreCreate = func(ctx context.Context, client *alipay.Client, param alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error) {
		preCreateCalls++
		if param.ProductCode != alipayProductCodePreCreate {
			t.Fatalf("product_code = %q, want %q", param.ProductCode, alipayProductCodePreCreate)
		}
		return &alipay.TradePreCreateRsp{
			Error:  alipay.Error{Code: alipay.CodeSuccess},
			QRCode: "https://qr.alipay.example.com/precreate-token",
		}, nil
	}
	alipayTradePagePay = func(client *alipay.Client, param alipay.TradePagePay) (*url.URL, error) {
		pagePayCalls++
		return url.Parse("https://openapi.alipay.com/gateway.do?page-pay")
	}

	provider := &Alipay{}
	resp, err := provider.createDesktopTrade(context.Background(), &alipay.Client{}, payment.CreatePaymentRequest{
		OrderID: "sub2_102",
		Amount:  "66.00",
		Subject: "Balance recharge",
	}, "https://merchant.example.com/api/v1/payment/webhook/alipay", "https://merchant.example.com/payment/result")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if preCreateCalls != 1 {
		t.Fatalf("precreate calls = %d, want 1", preCreateCalls)
	}
	if pagePayCalls != 0 {
		t.Fatalf("page pay calls = %d, want 0", pagePayCalls)
	}
	if resp.QRCode != "https://qr.alipay.example.com/precreate-token" {
		t.Fatalf("qr_code = %q", resp.QRCode)
	}
	if resp.PayURL != "" {
		t.Fatalf("pay_url = %q, want empty for precreate", resp.PayURL)
	}
}

func TestAlipayMerchantIdentityMetadata(t *testing.T) {
	t.Parallel()

	provider := &Alipay{
		config: map[string]string{
			"appId": "2021001234567890",
		},
	}

	metadata := provider.MerchantIdentityMetadata()
	if metadata["app_id"] != "2021001234567890" {
		t.Fatalf("app_id = %q, want %q", metadata["app_id"], "2021001234567890")
	}
}

func TestParseAlipayAmount(t *testing.T) {
	t.Parallel()

	amount, err := parseAlipayAmount("", "88.00", "77.00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if amount != 88 {
		t.Fatalf("amount = %v, want 88", amount)
	}

	if _, err := parseAlipayAmount("", "not-a-number"); err == nil {
		t.Fatal("expected error when no valid amount field exists")
	}
}

func TestAlipayRefundRequestNo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		orderID string
		amount  string
		want    string
	}{
		{
			name:    "sanitizes dots and dashes from amount",
			orderID: "sub2_1001",
			amount:  "12.34",
			want:    "sub2_1001-refund-1234",
		},
		{
			name:    "falls back to order id when amount is empty",
			orderID: "sub2_1001",
			amount:  "",
			want:    "sub2_1001-refund",
		},
		{
			name:    "falls back to order id when amount has no digits",
			orderID: "sub2_1001",
			amount:  "--",
			want:    "sub2_1001-refund",
		},
		{
			name:    "empty order id returns empty",
			orderID: "  ",
			amount:  "12.34",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := alipayRefundRequestNo(tt.orderID, tt.amount); got != tt.want {
				t.Fatalf("alipayRefundRequestNo(%q, %q) = %q, want %q", tt.orderID, tt.amount, got, tt.want)
			}
		})
	}
}

func TestAlipayRefundUsesDeterministicRequestNo(t *testing.T) {
	origRefund := alipayTradeRefund
	t.Cleanup(func() { alipayTradeRefund = origRefund })

	var outRequestNos []string
	alipayTradeRefund = func(_ context.Context, _ *alipay.Client, param alipay.TradeRefund) (*alipay.TradeRefundRsp, error) {
		if param.OutTradeNo == "" {
			t.Error("OutTradeNo must not be empty")
		}
		outRequestNos = append(outRequestNos, param.OutRequestNo)
		return &alipay.TradeRefundRsp{
			Error:      alipay.Error{Code: alipay.CodeSuccess},
			FundChange: alipayFundChangeYes,
		}, nil
	}

	provider := &Alipay{client: &alipay.Client{}}
	req := payment.RefundRequest{OrderID: "sub2_deterministic", Amount: "12.34", Reason: "user request"}

	first, err := provider.Refund(context.Background(), req)
	if err != nil {
		t.Fatalf("first refund error: %v", err)
	}
	second, err := provider.Refund(context.Background(), req)
	if err != nil {
		t.Fatalf("second refund error: %v", err)
	}

	if len(outRequestNos) != 2 {
		t.Fatalf("TradeRefund calls = %d, want 2", len(outRequestNos))
	}
	if outRequestNos[0] != outRequestNos[1] {
		t.Fatalf("OutRequestNo not deterministic: %q vs %q", outRequestNos[0], outRequestNos[1])
	}
	want := alipayRefundRequestNo(req.OrderID, req.Amount)
	if outRequestNos[0] != want {
		t.Fatalf("OutRequestNo = %q, want %q", outRequestNos[0], want)
	}
	if first.RefundID != outRequestNos[0] {
		t.Fatalf("first RefundID = %q, want OutRequestNo %q", first.RefundID, outRequestNos[0])
	}
	if second.RefundID != outRequestNos[1] {
		t.Fatalf("second RefundID = %q, want OutRequestNo %q", second.RefundID, outRequestNos[1])
	}
	if first.Status != payment.ProviderStatusSuccess {
		t.Fatalf("status = %q, want %q", first.Status, payment.ProviderStatusSuccess)
	}
}

func TestAlipayRefundRejectsMissingOrderID(t *testing.T) {
	origRefund := alipayTradeRefund
	t.Cleanup(func() { alipayTradeRefund = origRefund })

	called := false
	alipayTradeRefund = func(_ context.Context, _ *alipay.Client, _ alipay.TradeRefund) (*alipay.TradeRefundRsp, error) {
		called = true
		return nil, errors.New("should not be called")
	}

	provider := &Alipay{client: &alipay.Client{}}
	if _, err := provider.Refund(context.Background(), payment.RefundRequest{OrderID: "  ", Amount: "1.00"}); err == nil {
		t.Fatal("expected error for missing order id")
	}
	if called {
		t.Fatal("gateway must not be called with an empty OutTradeNo")
	}
}

func TestAlipayQueryRefund(t *testing.T) {
	origQuery := alipayTradeFastPayRefundQuery
	t.Cleanup(func() { alipayTradeFastPayRefundQuery = origQuery })

	tests := []struct {
		name    string
		req     payment.RefundQueryRequest
		rsp     *alipay.TradeFastPayRefundQueryRsp
		err     error
		want    string
		wantErr bool
	}{
		{
			name: "refund success maps to success",
			req:  payment.RefundQueryRequest{OrderID: "sub2_q1", RefundID: "sub2_q1-refund-1234"},
			rsp:  &alipay.TradeFastPayRefundQueryRsp{Error: alipay.Error{Code: alipay.CodeSuccess}, RefundStatus: alipayRefundStatusSuccess},
			want: payment.ProviderStatusSuccess,
		},
		{
			name: "missing refund status maps to pending",
			req:  payment.RefundQueryRequest{OrderID: "sub2_q2", RefundID: "sub2_q2-refund-1234"},
			rsp:  &alipay.TradeFastPayRefundQueryRsp{Error: alipay.Error{Code: alipay.CodeSuccess}},
			want: payment.ProviderStatusPending,
		},
		{
			name: "unknown refund status maps to pending",
			req:  payment.RefundQueryRequest{OrderID: "sub2_q3", RefundID: "sub2_q3-refund-1234"},
			rsp:  &alipay.TradeFastPayRefundQueryRsp{Error: alipay.Error{Code: alipay.CodeSuccess}, RefundStatus: "REFUND_PROCESSING"},
			want: payment.ProviderStatusPending,
		},
		{
			name: "trade not exist maps to failed without error",
			req:  payment.RefundQueryRequest{OrderID: "sub2_q4", RefundID: "sub2_q4-refund-1234"},
			err:  tradeNotExistError(),
			want: payment.ProviderStatusFailed,
		},
		{
			name:    "other sdk error is propagated",
			req:     payment.RefundQueryRequest{OrderID: "sub2_q5", RefundID: "sub2_q5-refund-1234"},
			err:     systemError(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotParam alipay.TradeFastPayRefundQuery
			alipayTradeFastPayRefundQuery = func(_ context.Context, _ *alipay.Client, param alipay.TradeFastPayRefundQuery) (*alipay.TradeFastPayRefundQueryRsp, error) {
				gotParam = param
				return tt.rsp, tt.err
			}

			provider := &Alipay{client: &alipay.Client{}}
			resp, err := provider.QueryRefund(context.Background(), tt.req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if resp != nil {
					t.Fatalf("response = %+v, want nil", resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotParam.OutTradeNo != tt.req.OrderID {
				t.Fatalf("OutTradeNo = %q, want %q", gotParam.OutTradeNo, tt.req.OrderID)
			}
			if gotParam.OutRequestNo != tt.req.RefundID {
				t.Fatalf("OutRequestNo = %q, want %q", gotParam.OutRequestNo, tt.req.RefundID)
			}
			if resp == nil {
				t.Fatal("response is nil")
			}
			if resp.Status != tt.want {
				t.Fatalf("status = %q, want %q", resp.Status, tt.want)
			}
			if resp.RefundID != tt.req.RefundID {
				t.Fatalf("RefundID = %q, want %q", resp.RefundID, tt.req.RefundID)
			}
		})
	}
}

func TestAlipayQueryRefundFallsBackToDerivedRequestNo(t *testing.T) {
	origQuery := alipayTradeFastPayRefundQuery
	t.Cleanup(func() { alipayTradeFastPayRefundQuery = origQuery })

	var gotParam alipay.TradeFastPayRefundQuery
	alipayTradeFastPayRefundQuery = func(_ context.Context, _ *alipay.Client, param alipay.TradeFastPayRefundQuery) (*alipay.TradeFastPayRefundQueryRsp, error) {
		gotParam = param
		return &alipay.TradeFastPayRefundQueryRsp{
			Error:        alipay.Error{Code: alipay.CodeSuccess},
			RefundStatus: alipayRefundStatusSuccess,
		}, nil
	}

	provider := &Alipay{client: &alipay.Client{}}
	resp, err := provider.QueryRefund(context.Background(), payment.RefundQueryRequest{
		OrderID: "sub2_fallback",
		Amount:  "12.34",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := alipayRefundRequestNo("sub2_fallback", "12.34")
	if gotParam.OutRequestNo != want {
		t.Fatalf("OutRequestNo = %q, want %q", gotParam.OutRequestNo, want)
	}
	if resp.RefundID != want {
		t.Fatalf("RefundID = %q, want %q", resp.RefundID, want)
	}
	if resp.Status != payment.ProviderStatusSuccess {
		t.Fatalf("status = %q, want %q", resp.Status, payment.ProviderStatusSuccess)
	}
}
