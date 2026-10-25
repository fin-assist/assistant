package account

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/asking"
)

func TestUnitUnavailableHasNoSensitiveDetails(t *testing.T) {
	err := askingError(asking.ErrUnavailable)
	st := status.Convert(err)
	if st.Code() != codes.Unavailable || len(st.Details()) != 0 || st.Message() != "Unavailable" {
		t.Fatalf("disabled-model refusal: %v", err)
	}
}
