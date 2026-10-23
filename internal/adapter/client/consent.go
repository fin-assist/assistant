package client

import (
	"context"
	"errors"
	"strings"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AssistantConsent reads the effective assistant consent from the user service.
// It never caches grants: the RPC reads primary so revocation affects the next call.
type AssistantConsent struct {
	client userv1.UserInternalServiceClient
}

// NewAssistantConsent accepts nil for an unconfigured peer; Granted then fails closed.
// The supplied client must use Dial so only this service's credentials travel to user.
func NewAssistantConsent(c userv1.UserInternalServiceClient) *AssistantConsent {
	return &AssistantConsent{client: c}
}

// Granted performs one system read with the explicit, verified user ID. A grant to
// an older document is a refusal, while an incomplete response is a dependency error.
func (c *AssistantConsent) Granted(ctx context.Context, user string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if c.client == nil {
		return false, errors.New("assistant consent peer is not configured")
	}
	if strings.TrimSpace(user) == "" {
		return false, errors.New("assistant consent requires a user")
	}
	response, err := c.client.GetAssistantConsentStatus(ctx, &userv1.GetAssistantConsentStatusRequest{UserId: &user})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled:
			return false, context.Canceled
		case errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded:
			return false, context.DeadlineExceeded
		default:
			return false, errors.New("assistant consent unavailable")
		}
	}
	consent := response.GetConsent()
	if consent == nil || consent.CurrentVersion == "" {
		return false, errors.New("invalid assistant consent response")
	}
	if !consent.Granted {
		return false, nil
	}
	if consent.Version == nil || *consent.Version == "" {
		return false, errors.New("invalid assistant consent response")
	}
	return *consent.Version == consent.CurrentVersion, nil
}
