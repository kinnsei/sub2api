//go:build unit

package service

import (
	"context"
	"strconv"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

func TestBuildPaymentOrderProviderSnapshot_ExcludesSensitiveConfig(t *testing.T) {
	t.Parallel()

	sel := &payment.InstanceSelection{
		InstanceID:     "12",
		ProviderKey:    payment.TypeWxpay,
		SupportedTypes: "wxpay,wxpay_direct",
		PaymentMode:    "popup",
		Config: map[string]string{
			"privateKey": "secret",
			"apiV3Key":   "secret-v3",
			"appId":      "wx-app-id",
		},
	}

	snapshot := buildPaymentOrderProviderSnapshot(sel, CreateOrderRequest{})
	require.Equal(t, map[string]any{
		"schema_version":       2,
		"provider_instance_id": "12",
		"provider_key":         payment.TypeWxpay,
		"payment_mode":         "popup",
		"merchant_app_id":      "wx-app-id",
		"currency":             "CNY",
	}, snapshot)
	require.NotContains(t, snapshot, "config")
	require.NotContains(t, snapshot, "privateKey")
	require.NotContains(t, snapshot, "apiV3Key")
	require.NotContains(t, snapshot, "supported_types")
	require.NotContains(t, snapshot, "instance_name")
	require.NotContains(t, snapshot, "merchant_id")
}

func TestCreateOrderInTx_WritesProviderSnapshot(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)

	user, err := client.User.Create().
		SetEmail("snapshot@example.com").
		SetPasswordHash("hash").
		SetUsername("snapshot-user").
		Save(ctx)
	require.NoError(t, err)

	instance, err := client.PaymentProviderInstance.Create().
		SetProviderKey(payment.TypeAlipay).
		SetName("Primary Alipay").
		SetConfig(`{"secretKey":"do-not-copy"}`).
		SetSupportedTypes("alipay,alipay_direct").
		SetPaymentMode("redirect").
		SetEnabled(true).
		Save(ctx)
	require.NoError(t, err)

	svc := &PaymentService{entClient: client}
	order, err := svc.createOrderInTx(
		ctx,
		CreateOrderRequest{
			UserID:      user.ID,
			PaymentType: payment.TypeAlipay,
			OrderType:   payment.OrderTypeBalance,
			ClientIP:    "127.0.0.1",
			SrcHost:     "app.example.com",
		},
		&User{
			ID:       user.ID,
			Email:    user.Email,
			Username: user.Username,
		},
		nil,
		&PaymentConfig{
			MaxPendingOrders: 3,
			OrderTimeoutMin:  30,
		},
		88,
		88,
		0,
		88,
		&payment.InstanceSelection{
			InstanceID:     strconv.FormatInt(instance.ID, 10),
			ProviderKey:    payment.TypeAlipay,
			SupportedTypes: "alipay,alipay_direct",
			PaymentMode:    "redirect",
			Config: map[string]string{
				"secretKey": "do-not-copy",
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(instance.ID, 10), valueOrEmpty(order.ProviderInstanceID))
	require.Equal(t, payment.TypeAlipay, valueOrEmpty(order.ProviderKey))
	require.Equal(t, float64(2), order.ProviderSnapshot["schema_version"])
	require.Equal(t, strconv.FormatInt(instance.ID, 10), order.ProviderSnapshot["provider_instance_id"])
	require.Equal(t, payment.TypeAlipay, order.ProviderSnapshot["provider_key"])
	require.Equal(t, "redirect", order.ProviderSnapshot["payment_mode"])
	require.NotContains(t, order.ProviderSnapshot, "config")
	require.NotContains(t, order.ProviderSnapshot, "secretKey")
	require.NotContains(t, order.ProviderSnapshot, "supported_types")
	require.NotContains(t, order.ProviderSnapshot, "instance_name")
}

func TestBuildPaymentOrderProviderSnapshot_UsesWxpayJSAPIAppIDForOpenIDOrders(t *testing.T) {
	t.Parallel()

	snapshot := buildPaymentOrderProviderSnapshot(&payment.InstanceSelection{
		InstanceID:  "88",
		ProviderKey: payment.TypeWxpay,
		Config: map[string]string{
			"appId":   "wx-open-app",
			"mpAppId": "wx-mp-app",
			"mchId":   "mch-88",
		},
		PaymentMode: "jsapi",
	}, CreateOrderRequest{OpenID: "openid-123"})

	require.Equal(t, "wx-mp-app", snapshot["merchant_app_id"])
	require.Equal(t, "mch-88", snapshot["merchant_id"])
	require.Equal(t, "CNY", snapshot["currency"])
}

func TestBuildPaymentOrderProviderSnapshot_IncludesAlipayMerchantIdentity(t *testing.T) {
	t.Parallel()

	snapshot := buildPaymentOrderProviderSnapshot(&payment.InstanceSelection{
		InstanceID:  "21",
		ProviderKey: payment.TypeAlipay,
		Config: map[string]string{
			"appId":      "alipay-app-21",
			"privateKey": "secret",
		},
		PaymentMode: "redirect",
	}, CreateOrderRequest{})

	require.Equal(t, "alipay-app-21", snapshot["merchant_app_id"])
	require.NotContains(t, snapshot, "privateKey")
}

func TestBuildPaymentOrderProviderSnapshot_IncludesEasyPayMerchantIdentity(t *testing.T) {
	t.Parallel()

	snapshot := buildPaymentOrderProviderSnapshot(&payment.InstanceSelection{
		InstanceID:  "66",
		ProviderKey: payment.TypeEasyPay,
		Config: map[string]string{
			"pid":  "easypay-merchant-66",
			"pkey": "secret",
		},
		PaymentMode: "popup",
	}, CreateOrderRequest{PaymentType: payment.TypeAlipay})

	require.Equal(t, "easypay-merchant-66", snapshot["merchant_id"])
	require.NotContains(t, snapshot, "pkey")
}

func TestBuildPaymentOrderProviderSnapshot_IncludesProviderCurrency(t *testing.T) {
	t.Parallel()

	stripeSnapshot := buildPaymentOrderProviderSnapshot(&payment.InstanceSelection{
		InstanceID:  "77",
		ProviderKey: payment.TypeStripe,
		Config: map[string]string{
			"currency": "hkd",
		},
	}, CreateOrderRequest{})
	require.Equal(t, "HKD", stripeSnapshot["currency"])

	airwallexSnapshot := buildPaymentOrderProviderSnapshot(&payment.InstanceSelection{
		InstanceID:  "78",
		ProviderKey: payment.TypeAirwallex,
		Config: map[string]string{
			"currency":  "usd",
			"accountId": "acct-78",
		},
	}, CreateOrderRequest{})
	require.Equal(t, "USD", airwallexSnapshot["currency"])
	require.Equal(t, "acct-78", airwallexSnapshot["merchant_id"])
}

// ---------------------------------------------------------------------------
// wxpay merchant identity parity with the stored snapshot
// ---------------------------------------------------------------------------

// wxpaySnapshotOrder builds the order row the refund path validates against.
func wxpaySnapshotOrder(merchantAppID string) *dbent.PaymentOrder {
	return &dbent.PaymentOrder{
		PaymentType: payment.TypeWxpay,
		ProviderSnapshot: map[string]any{
			"schema_version":  2,
			"merchant_app_id": merchantAppID,
			"merchant_id":     "1900000001",
			"currency":        "CNY",
		},
	}
}

func TestValidateProviderSnapshotMetadata_WxpayIdentityMetadataMatchesSnapshot(t *testing.T) {
	t.Parallel()

	t.Run("refund identity of a native order passes", func(t *testing.T) {
		t.Parallel()
		order := wxpaySnapshotOrder("wx-merchant-app")
		identity := map[string]string{
			"appid":    "wx-merchant-app",
			"mchid":    "1900000001",
			"currency": "CNY",
		}
		require.NoError(t, validateProviderSnapshotMetadata(order, payment.TypeWxpay, identity))
	})

	t.Run("refund identity of a JSAPI order passes via mp_appid", func(t *testing.T) {
		t.Parallel()
		// The order settled under the dedicated MP AppID, so the snapshot holds
		// mpAppId while the provider identity reports appid=base, mp_appid=mp.
		order := wxpaySnapshotOrder("wx-mp-app")
		identity := map[string]string{
			"appid":    "wx-merchant-app",
			"mp_appid": "wx-mp-app",
			"mchid":    "1900000001",
			"currency": "CNY",
		}
		require.NoError(t, validateProviderSnapshotMetadata(order, payment.TypeWxpay, identity))
	})

	t.Run("a genuinely different app id is still rejected", func(t *testing.T) {
		t.Parallel()
		order := wxpaySnapshotOrder("wx-mp-app")
		identity := map[string]string{
			"appid":    "wx-some-other-app",
			"mp_appid": "wx-another-mp-app",
			"mchid":    "1900000001",
			"currency": "CNY",
		}
		err := validateProviderSnapshotMetadata(order, payment.TypeWxpay, identity)
		require.Error(t, err)
		require.Contains(t, err.Error(), "wxpay appid mismatch")
	})

	t.Run("same base app id but different MP app id is rejected", func(t *testing.T) {
		t.Parallel()
		// Two instances may share a merchant appid while using different MP
		// AppIDs. The MP AppID is the discriminator, so accepting the base appid
		// alone would refund a JSAPI order through the wrong merchant.
		order := wxpaySnapshotOrder("wx-mp-app")
		identity := map[string]string{
			"appid":    "wx-merchant-app",
			"mp_appid": "wx-other-mp-app",
			"mchid":    "1900000001",
			"currency": "CNY",
		}
		err := validateProviderSnapshotMetadata(order, payment.TypeWxpay, identity)
		require.Error(t, err)
		require.Contains(t, err.Error(), "wxpay appid mismatch")
	})

	t.Run("provider identity supplies appid, snapshot keeps strict webhook semantics", func(t *testing.T) {
		t.Parallel()
		// Webhook metadata never carries mp_appid, so a JSAPI-settled order whose
		// notification reports the base AppID would now be a mismatch. That is
		// pre-existing behaviour and is asserted here so it cannot regress silently.
		order := wxpaySnapshotOrder("wx-mp-app")
		err := validateProviderSnapshotMetadata(order, payment.TypeWxpay, map[string]string{
			"appid":       "wx-merchant-app",
			"mchid":       "1900000001",
			"currency":    "CNY",
			"trade_state": "SUCCESS",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "wxpay appid mismatch")
	})
}

func valueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
