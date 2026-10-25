package account

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// unavailable preserves the existing service refusal for a disabled model or unavailable
// dependency before admission. It carries no contract details or internal error text.
func unavailable() error {
	return status.Error(codes.Unavailable, codes.Unavailable.String()) //nolint:forbidigo // Bare internal refusal, grpc-api.md 5.1 rule 3; preserves the disabled-model contract.
}
