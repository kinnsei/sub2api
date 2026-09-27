//go:build unit

package provider

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/smartwalle/alipay/v3"
)

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
			name: "error containing ACQ.TRADE_NOT_EXIST returns true",
			err:  errors.New("alipay: sub_code=ACQ.TRADE_NOT_EXIST, sub_msg=交易不存在"),
			want: true,
		},
		{
			name: "error not containing the code returns false",
			err:  errors.New("alipay: sub_code=ACQ.SYSTEM_ERROR, sub_msg=系统错误"),
			want: false,
		},
		{
			name: "error with only partial match returns false",
			err:  errors.New("ACQ.TRADE_NOT"),
			want: false,
		},
		{
			name: "error with exact constant value returns true",
			err:  errors.New(alipayErrTradeNotExist),
			want: true,
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
			rsp:  &alipay.TradeFastPayRefundQueryRsp{RefundStatus: alipayRefundStatusSuccess},
			want: payment.ProviderStatusSuccess,
		},
		{
			name: "missing refund status maps to pending",
			req:  payment.RefundQueryRequest{OrderID: "sub2_q2", RefundID: "sub2_q2-refund-1234"},
			rsp:  &alipay.TradeFastPayRefundQueryRsp{},
			want: payment.ProviderStatusPending,
		},
		{
			name: "unknown refund status maps to pending",
			req:  payment.RefundQueryRequest{OrderID: "sub2_q3", RefundID: "sub2_q3-refund-1234"},
			rsp:  &alipay.TradeFastPayRefundQueryRsp{RefundStatus: "REFUND_PROCESSING"},
			want: payment.ProviderStatusPending,
		},
		{
			name: "trade not exist maps to failed without error",
			req:  payment.RefundQueryRequest{OrderID: "sub2_q4", RefundID: "sub2_q4-refund-1234"},
			err:  errors.New("alipay: sub_code=ACQ.TRADE_NOT_EXIST, sub_msg=交易不存在"),
			want: payment.ProviderStatusFailed,
		},
		{
			name:    "other sdk error is propagated",
			req:     payment.RefundQueryRequest{OrderID: "sub2_q5", RefundID: "sub2_q5-refund-1234"},
			err:     errors.New("alipay: sub_code=ACQ.SYSTEM_ERROR"),
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
		return &alipay.TradeFastPayRefundQueryRsp{RefundStatus: alipayRefundStatusSuccess}, nil
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
