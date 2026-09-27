//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// loadBalancerErrorStub returns a fixed SelectInstance error so the create-order
// error mapping can be asserted without a database.
type loadBalancerErrorStub struct {
	err error
}

func (s *loadBalancerErrorStub) GetInstanceConfig(context.Context, int64) (map[string]string, error) {
	return nil, errors.New("unexpected call")
}

func (s *loadBalancerErrorStub) SelectInstance(context.Context, string, payment.PaymentType, payment.Strategy, float64) (*payment.InstanceSelection, error) {
	return nil, s.err
}

func TestSelectCreateOrderInstanceMapsLimitsExceededToNoAvailableInstance(t *testing.T) {
	svc := &PaymentService{
		loadBalancer: &loadBalancerErrorStub{err: payment.ErrInstanceLimitsExceeded},
	}

	sel, err := svc.selectCreateOrderInstance(context.Background(), CreateOrderRequest{
		UserID:      1,
		PaymentType: payment.TypeAlipay,
		Amount:      100,
	}, &PaymentConfig{}, 100)
	require.Nil(t, sel)
	require.Error(t, err)
	require.Equal(t, "NO_AVAILABLE_INSTANCE", infraerrors.Reason(err))
}

func TestSelectCreateOrderInstanceMapsUsageFailureToGatewayError(t *testing.T) {
	svc := &PaymentService{
		loadBalancer: &loadBalancerErrorStub{err: payment.ErrInstanceUsageUnavailable},
	}

	sel, err := svc.selectCreateOrderInstance(context.Background(), CreateOrderRequest{
		UserID:      1,
		PaymentType: payment.TypeAlipay,
		Amount:      100,
	}, &PaymentConfig{}, 100)
	require.Nil(t, sel)
	require.Error(t, err)
	require.Equal(t, "PAYMENT_GATEWAY_ERROR", infraerrors.Reason(err))
}
