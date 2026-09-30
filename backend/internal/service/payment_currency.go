package service

import (
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func paymentProviderConfigCurrency(providerKey string, cfg map[string]string) string {
	switch strings.TrimSpace(providerKey) {
	case payment.TypeStripe, payment.TypeAirwallex:
		currency, err := payment.NormalizePaymentCurrency(cfg["currency"])
		if err == nil {
			return currency
		}
	}
	return payment.DefaultPaymentCurrency
}

// PaymentOrderCurrency returns the settlement currency of an order.
//
// The currency column is authoritative: it is frozen at order creation for every
// channel. Older rows predate the column (migration 256 backfilled them from the
// snapshot, or the default when the snapshot had no currency), so the snapshot and
// finally the default remain as fallbacks for robustness.
//
// An empty column must fall through to the snapshot rather than be normalized:
// NormalizePaymentCurrency("") returns the default, which would otherwise hide a
// snapshot currency (e.g. USD) on any row written before the backfill ran.
func PaymentOrderCurrency(order *dbent.PaymentOrder) string {
	if order != nil && strings.TrimSpace(order.Currency) != "" {
		if currency, err := payment.NormalizePaymentCurrency(order.Currency); err == nil {
			return currency
		}
	}
	if snapshot := psOrderProviderSnapshot(order); snapshot != nil {
		if currency, err := payment.NormalizePaymentCurrency(snapshot.Currency); err == nil {
			return currency
		}
	}
	return payment.DefaultPaymentCurrency
}
