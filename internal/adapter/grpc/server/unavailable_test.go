package server

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUnavailableHasNoSensitiveDetails(t *testing.T) {
	err := Unavailable()
	if status.Code(err) != codes.Unavailable || Reason(err) != "" || len(status.Convert(err).Details()) != 0 || status.Convert(err).Message() != "Unavailable" {
		t.Fatal(err)
	}
	if status.Code(normalize(err)) != codes.Unavailable {
		t.Fatal("normalizer lost retryable infrastructure status")
	}
}
