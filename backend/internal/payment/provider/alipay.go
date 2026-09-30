package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/smartwalle/alipay/v3"
)

// alipayHTTPTimeout bounds every gateway call. The SDK defaults the client to
// http.DefaultClient, which has no timeout at all.
const alipayHTTPTimeout = 15 * time.Second

// Alipay product codes.
const (
	alipayProductCodePreCreate = "FACE_TO_FACE_PAYMENT"
	alipayProductCodeWapPay    = "QUICK_WAP_WAY"
	alipayProductCodePagePay   = "FAST_INSTANT_TRADE_PAY"
)

// Alipay response constants.
const (
	alipayFundChangeYes       = "Y"
	alipayErrTradeNotExist    = "ACQ.TRADE_NOT_EXIST"
	alipayRefundSuffix        = "-refund"
	alipayRefundStatusSuccess = "REFUND_SUCCESS"
)

var (
	alipayTradeWapPay = func(client *alipay.Client, param alipay.TradeWapPay) (*url.URL, error) {
		return client.TradeWapPay(param)
	}
	alipayTradePreCreate = func(ctx context.Context, client *alipay.Client, param alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error) {
		return client.TradePreCreate(ctx, param)
	}
	alipayTradePagePay = func(client *alipay.Client, param alipay.TradePagePay) (*url.URL, error) {
		return client.TradePagePay(param)
	}
	alipayTradeRefund = func(ctx context.Context, client *alipay.Client, param alipay.TradeRefund) (*alipay.TradeRefundRsp, error) {
		return client.TradeRefund(ctx, param)
	}
	alipayTradeFastPayRefundQuery = func(ctx context.Context, client *alipay.Client, param alipay.TradeFastPayRefundQuery) (*alipay.TradeFastPayRefundQueryRsp, error) {
		return client.TradeFastPayRefundQuery(ctx, param)
	}
)

// Alipay implements payment.Provider and payment.CancelableProvider using the smartwalle/alipay SDK.
type Alipay struct {
	instanceID string
	config     map[string]string // appId, privateKey, publicKey (or alipayPublicKey), notifyUrl, returnUrl

	mu     sync.Mutex
	client *alipay.Client
}

// NewAlipay creates a new Alipay provider instance.
func NewAlipay(instanceID string, config map[string]string) (*Alipay, error) {
	required := []string{"appId", "privateKey"}
	for _, k := range required {
		if config[k] == "" {
			return nil, fmt.Errorf("alipay config missing required key: %s", k)
		}
	}
	return &Alipay{
		instanceID: instanceID,
		config:     config,
	}, nil
}

func (a *Alipay) getClient() (*alipay.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return a.client, nil
	}
	client, err := alipay.New(
		a.config["appId"],
		a.config["privateKey"],
		true,
		alipay.WithHTTPClient(&http.Client{
			Timeout:   alipayHTTPTimeout,
			Transport: http.DefaultTransport,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("alipay init client: %w", err)
	}
	pubKey := a.config["publicKey"]
	if pubKey == "" {
		pubKey = a.config["alipayPublicKey"]
	}
	if pubKey == "" {
		return nil, fmt.Errorf("alipay config missing required key: publicKey (or alipayPublicKey)")
	}
	if err := client.LoadAliPayPublicKey(pubKey); err != nil {
		return nil, fmt.Errorf("alipay load public key: %w", err)
	}
	a.client = client
	return a.client, nil
}

func (a *Alipay) Name() string        { return "Alipay" }
func (a *Alipay) ProviderKey() string { return payment.TypeAlipay }
func (a *Alipay) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeAlipay}
}

func (a *Alipay) MerchantIdentityMetadata() map[string]string {
	if a == nil {
		return nil
	}
	appID := strings.TrimSpace(a.config["appId"])
	if appID == "" {
		return nil
	}
	return map[string]string{"app_id": appID}
}

// CreatePayment creates an Alipay payment using the following routing:
//   - Mobile (H5), default: alipay.trade.wap.pay — browser redirect into Alipay.
//   - Mobile with AlipayMobilePrecreate: alipay.trade.precreate — return the
//     dynamic QR payload so the frontend can open it through the Alipay app.
//   - Desktop, default: prefer alipay.trade.precreate (FACE_TO_FACE_PAYMENT) to
//     get a scannable QR payload. If precreate is unavailable for the merchant,
//     fall back to alipay.trade.page.pay and expose pay_url only — the frontend
//     opens the Alipay checkout in a new tab.
//   - Desktop, paymentMode == "redirect": skip precreate and go straight to
//     alipay.trade.page.pay so the frontend always opens the Alipay checkout
//     in a new tab. Use this when the merchant has not enabled FACE_TO_FACE_PAYMENT.
//
// Note: alipay.trade.page.pay returns a checkout page URL, not a scannable
// payment QR. Never expose it via the QRCode field.
func (a *Alipay) CreatePayment(ctx context.Context, req payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	client, err := a.getClient()
	if err != nil {
		return nil, err
	}

	notifyURL := a.config["notifyUrl"]
	if req.NotifyURL != "" {
		notifyURL = req.NotifyURL
	}
	returnURL := a.config["returnUrl"]
	if req.ReturnURL != "" {
		returnURL = req.ReturnURL
	}

	if req.IsMobile {
		if req.AlipayMobilePrecreate {
			return a.createPrecreateTrade(ctx, client, req, notifyURL)
		}
		return a.createWapTrade(client, req, notifyURL, returnURL)
	}
	return a.createDesktopTrade(ctx, client, req, notifyURL, returnURL)
}

func (a *Alipay) createWapTrade(client *alipay.Client, req payment.CreatePaymentRequest, notifyURL, returnURL string) (*payment.CreatePaymentResponse, error) {
	param := alipay.TradeWapPay{}
	param.OutTradeNo = req.OrderID
	param.TotalAmount = req.Amount
	param.Subject = req.Subject
	param.ProductCode = alipayProductCodeWapPay
	param.NotifyURL = notifyURL
	param.ReturnURL = returnURL

	payURL, err := alipayTradeWapPay(client, param)
	if err != nil {
		return nil, fmt.Errorf("alipay TradeWapPay: %w", err)
	}
	return &payment.CreatePaymentResponse{
		TradeNo: req.OrderID,
		PayURL:  payURL.String(),
	}, nil
}

func (a *Alipay) createDesktopTrade(ctx context.Context, client *alipay.Client, req payment.CreatePaymentRequest, notifyURL, returnURL string) (*payment.CreatePaymentResponse, error) {
	// Explicit redirect mode: merchant opted into "always open the Alipay
	// checkout page in a new tab" via the provider instance's payment_mode.
	// Skip precreate to avoid a wasted API call.
	if strings.EqualFold(strings.TrimSpace(a.config["paymentMode"]), "redirect") {
		return a.createPagePayTrade(client, req, notifyURL, returnURL)
	}

	resp, precreateErr := a.createPrecreateTrade(ctx, client, req, notifyURL)
	if precreateErr == nil {
		return resp, nil
	}

	resp, pagePayErr := a.createPagePayTrade(client, req, notifyURL, returnURL)
	if pagePayErr == nil {
		return resp, nil
	}

	return nil, fmt.Errorf("alipay desktop payment failed: precreate=%v; pagepay=%w", precreateErr, pagePayErr)
}

func (a *Alipay) createPrecreateTrade(ctx context.Context, client *alipay.Client, req payment.CreatePaymentRequest, notifyURL string) (*payment.CreatePaymentResponse, error) {
	param := alipay.TradePreCreate{}
	param.OutTradeNo = req.OrderID
	param.TotalAmount = req.Amount
	param.Subject = req.Subject
	param.ProductCode = alipayProductCodePreCreate
	param.NotifyURL = notifyURL

	rsp, err := alipayTradePreCreate(ctx, client, param)
	if err != nil {
		return nil, fmt.Errorf("alipay TradePreCreate: %w", err)
	}
	if rsp == nil {
		return nil, fmt.Errorf("alipay TradePreCreate: empty response")
	}
	if rsp.IsFailure() {
		return nil, fmt.Errorf("alipay TradePreCreate failed: %s", rsp.Error.Error())
	}
	if strings.TrimSpace(rsp.QRCode) == "" {
		return nil, fmt.Errorf("alipay TradePreCreate: empty qr_code")
	}

	return &payment.CreatePaymentResponse{
		TradeNo: req.OrderID,
		QRCode:  rsp.QRCode,
	}, nil
}

func (a *Alipay) createPagePayTrade(client *alipay.Client, req payment.CreatePaymentRequest, notifyURL, returnURL string) (*payment.CreatePaymentResponse, error) {
	param := alipay.TradePagePay{}
	param.OutTradeNo = req.OrderID
	param.TotalAmount = req.Amount
	param.Subject = req.Subject
	param.ProductCode = alipayProductCodePagePay
	param.NotifyURL = notifyURL
	param.ReturnURL = returnURL

	payURL, err := alipayTradePagePay(client, param)
	if err != nil {
		return nil, fmt.Errorf("alipay TradePagePay: %w", err)
	}
	// Only PayURL is exposed: alipay.trade.page.pay returns a checkout page URL
	// that must be opened in a browser, not a scannable payment QR. Setting it
	// as QRCode would let the frontend render an unscannable image.
	return &payment.CreatePaymentResponse{
		TradeNo: req.OrderID,
		PayURL:  payURL.String(),
	}, nil
}

// QueryOrder queries the trade status via Alipay.
func (a *Alipay) QueryOrder(ctx context.Context, tradeNo string) (*payment.QueryOrderResponse, error) {
	client, err := a.getClient()
	if err != nil {
		return nil, err
	}

	result, err := client.TradeQuery(ctx, alipay.TradeQuery{OutTradeNo: tradeNo})
	if err != nil {
		if isTradeNotExist(err) {
			// Alipay does not know this trade. That is "not paid yet", not a
			// hard failure, and the caller dereferences the response.
			return &payment.QueryOrderResponse{
				TradeNo:  tradeNo,
				Status:   payment.ProviderStatusPending,
				Metadata: a.MerchantIdentityMetadata(),
			}, nil
		}
		return nil, fmt.Errorf("alipay TradeQuery: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("alipay TradeQuery: empty response")
	}

	status := payment.ProviderStatusPending
	statusKnown := false
	switch result.TradeStatus {
	case alipay.TradeStatusSuccess, alipay.TradeStatusFinished:
		status = payment.ProviderStatusPaid
		statusKnown = true
	case alipay.TradeStatusClosed:
		status = payment.ProviderStatusFailed
		statusKnown = true
	}

	amount, amountErr := strconv.ParseFloat(strings.TrimSpace(result.TotalAmount), 64)
	if amountErr != nil {
		amount, amountErr = parseAlipayAmount(
			result.TotalAmount,
			result.ReceiptAmount,
			result.BuyerPayAmount,
			result.InvoiceAmount,
		)
	}
	if amountErr != nil {
		if !statusKnown {
			// Alipay answers a query for a trade it never created with code 40004
			// and an otherwise empty body: no trade_status and no amount fields.
			// That is "not paid yet", not a parse failure. A trade that does carry
			// a real status (success/closed) must still surface the error rather
			// than be silently downgraded to pending.
			return &payment.QueryOrderResponse{
				TradeNo:  tradeNo,
				Status:   payment.ProviderStatusPending,
				Metadata: a.MerchantIdentityMetadata(),
			}, nil
		}
		return nil, fmt.Errorf("alipay parse amount: %w", amountErr)
	}

	return &payment.QueryOrderResponse{
		TradeNo:  result.TradeNo,
		Status:   status,
		Amount:   amount,
		PaidAt:   result.SendPayDate,
		Metadata: a.MerchantIdentityMetadata(),
	}, nil
}

// VerifyNotification decodes and verifies an Alipay async notification.
func (a *Alipay) VerifyNotification(ctx context.Context, rawBody string, _ map[string]string) (*payment.PaymentNotification, error) {
	client, err := a.getClient()
	if err != nil {
		return nil, err
	}

	values, err := url.ParseQuery(rawBody)
	if err != nil {
		return nil, fmt.Errorf("alipay parse notification: %w", err)
	}

	notification, err := client.DecodeNotification(ctx, values)
	if err != nil {
		return nil, fmt.Errorf("alipay verify notification: %w", err)
	}
	if notification == nil {
		return nil, fmt.Errorf("alipay verify notification: empty notification")
	}

	status := payment.ProviderStatusFailed
	if notification.TradeStatus == alipay.TradeStatusSuccess || notification.TradeStatus == alipay.TradeStatusFinished {
		status = payment.ProviderStatusSuccess
	}

	amount, err := strconv.ParseFloat(strings.TrimSpace(notification.TotalAmount), 64)
	if err != nil {
		amount, err = parseAlipayAmount(
			notification.TotalAmount,
			notification.ReceiptAmount,
			notification.BuyerPayAmount,
		)
		if err != nil {
			return nil, fmt.Errorf("alipay parse notification amount: %w", err)
		}
	}

	metadata := a.MerchantIdentityMetadata()
	if appID := strings.TrimSpace(notification.AppId); appID != "" {
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata["app_id"] = appID
	}

	return &payment.PaymentNotification{
		TradeNo:  notification.TradeNo,
		OrderID:  notification.OutTradeNo,
		Amount:   amount,
		Status:   status,
		RawData:  rawBody,
		Metadata: metadata,
	}, nil
}

// Refund requests a refund through Alipay.
func (a *Alipay) Refund(ctx context.Context, req payment.RefundRequest) (*payment.RefundResponse, error) {
	if strings.TrimSpace(req.OrderID) == "" {
		return nil, fmt.Errorf("alipay refund missing order id")
	}

	client, err := a.getClient()
	if err != nil {
		return nil, err
	}

	// An explicit refund number wins: it is the value the caller persisted when
	// it first asked for this refund, so reuse it for idempotency and lookups.
	outRequestNo := strings.TrimSpace(req.RefundNo)
	if outRequestNo == "" {
		outRequestNo = alipayRefundRequestNo(req.OrderID, req.Amount)
	}
	result, err := alipayTradeRefund(ctx, client, alipay.TradeRefund{
		OutTradeNo:   req.OrderID,
		RefundAmount: req.Amount,
		RefundReason: req.Reason,
		OutRequestNo: outRequestNo,
	})
	if err != nil {
		return nil, fmt.Errorf("alipay TradeRefund: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("alipay TradeRefund: empty response")
	}
	// A business failure (bad amount, trade already refunded, ...) arrives as a
	// nil Go error with a non-success code. Reporting it as "pending" would make
	// the caller record a refund that never reached the gateway.
	if result.IsFailure() {
		return nil, fmt.Errorf("alipay TradeRefund failed: %s", result.Error.Error())
	}

	refundStatus := payment.ProviderStatusPending
	if result.FundChange == alipayFundChangeYes {
		refundStatus = payment.ProviderStatusSuccess
	}

	return &payment.RefundResponse{
		RefundID: outRequestNo,
		Status:   refundStatus,
	}, nil
}

// QueryRefund queries the status of a previously requested Alipay refund.
func (a *Alipay) QueryRefund(ctx context.Context, req payment.RefundQueryRequest) (*payment.RefundResponse, error) {
	client, err := a.getClient()
	if err != nil {
		return nil, err
	}

	outRequestNo := strings.TrimSpace(req.RefundID)
	if outRequestNo == "" {
		outRequestNo = alipayRefundRequestNo(req.OrderID, req.Amount)
	}

	result, err := alipayTradeFastPayRefundQuery(ctx, client, alipay.TradeFastPayRefundQuery{
		OutTradeNo:   req.OrderID,
		OutRequestNo: outRequestNo,
	})
	if err != nil {
		if isTradeNotExist(err) {
			return &payment.RefundResponse{
				RefundID: outRequestNo,
				Status:   payment.ProviderStatusFailed,
			}, nil
		}
		return nil, fmt.Errorf("alipay query refund: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("alipay query refund: empty response")
	}
	// Gateway answered without an error object but with a failure code (e.g. the
	// out_request_no does not belong to this trade). Treating that as "pending"
	// would leave the refund waiting forever on a request the gateway rejected.
	if result.IsFailure() {
		return nil, fmt.Errorf("alipay query refund failed: %s", result.Error.Error())
	}

	status := payment.ProviderStatusPending
	if result.RefundStatus == alipayRefundStatusSuccess {
		status = payment.ProviderStatusSuccess
	}

	return &payment.RefundResponse{
		RefundID: outRequestNo,
		Status:   status,
	}, nil
}

// alipayRefundRequestNo derives a deterministic, idempotent refund request
// number so a refund can be looked up later via out_request_no.
func alipayRefundRequestNo(orderID, amount string) string {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return ""
	}
	amount = strings.NewReplacer(".", "", "-", "").Replace(strings.TrimSpace(amount))
	if amount == "" {
		return orderID + alipayRefundSuffix
	}
	return orderID + alipayRefundSuffix + "-" + amount
}

// CancelPayment closes a pending trade on Alipay.
func (a *Alipay) CancelPayment(ctx context.Context, tradeNo string) error {
	client, err := a.getClient()
	if err != nil {
		return err
	}

	result, err := client.TradeClose(ctx, alipay.TradeClose{OutTradeNo: tradeNo})
	if err != nil {
		if isTradeNotExist(err) {
			// Already gone upstream: closing is idempotent, so this is a success.
			return nil
		}
		return fmt.Errorf("alipay TradeClose: %w", err)
	}
	// A business failure here arrives as a nil Go error. Treating a rejected
	// close as success would leave the caller believing the trade can no longer
	// be paid, when in fact it is still open.
	if result != nil && result.IsFailure() {
		if result.SubCode == alipayErrTradeNotExist {
			return nil
		}
		return fmt.Errorf("alipay TradeClose failed: %s", result.Error.Error())
	}
	return nil
}

// isTradeNotExist reports whether err is an Alipay gateway error whose sub_code
// is ACQ.TRADE_NOT_EXIST.
//
// The SDK returns *alipay.Error from Client.decode for every gateway-level
// failure, and alipay.Error.Error() renders only "<code> - <sub_msg>" — the
// sub_code is never part of the message. Matching on the rendered string can
// therefore never succeed, so the structured error is inspected instead.
func isTradeNotExist(err error) bool {
	var apiErr *alipay.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr != nil && apiErr.SubCode == alipayErrTradeNotExist
}

func parseAlipayAmount(values ...string) (float64, error) {
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		amount, err := strconv.ParseFloat(raw, 64)
		if err == nil {
			return amount, nil
		}
	}
	return 0, fmt.Errorf("no valid amount field")
}

// Ensure interface compliance.
var (
	_ payment.Provider                 = (*Alipay)(nil)
	_ payment.CancelableProvider       = (*Alipay)(nil)
	_ payment.MerchantIdentityProvider = (*Alipay)(nil)
	_ payment.RefundQueryProvider      = (*Alipay)(nil)
)
