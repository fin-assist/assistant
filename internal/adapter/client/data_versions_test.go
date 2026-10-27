package client

import (
	"context"
	"errors"
	"testing"
	"time"

	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/assistant/internal/dialogue"
)

type versionClient struct {
	statementv1.AssistantToolsServiceClient
	response *statementv1.GetDataVersionResponse
	err      error
	user     string
}

func (c *versionClient) GetDataVersion(_ context.Context, r *statementv1.GetDataVersionRequest, _ ...grpc.CallOption) (*statementv1.GetDataVersionResponse, error) {
	c.user = r.UserId
	return c.response, c.err
}
func TestUnitDataVersions(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	peer := &versionClient{response: &statementv1.GetDataVersionResponse{Version: &statementv1.DataVersion{CategoriesChangedAt: timestamppb.New(now), UploadsChangedAt: timestamppb.New(now)}}}
	c := NewDataVersions(peer)
	result, err := c.Get(t.Context(), "u")
	if err != nil || !result.Categories.Equal(now) || !result.Uploads.Equal(now) || peer.user != "u" {
		t.Fatalf("%+v %v", result, err)
	}
	peer.response = &statementv1.GetDataVersionResponse{}
	result, err = c.Get(t.Context(), "u")
	if err != nil || result.Categories != nil || result.Uploads != nil {
		t.Fatalf("%+v %v", result, err)
	}
	for _, version := range []*statementv1.DataVersion{{CategoriesChangedAt: &timestamppb.Timestamp{Seconds: 1 << 62}}, {UploadsChangedAt: &timestamppb.Timestamp{Seconds: 1 << 62}}} {
		peer.response.Version = version
		if _, err := c.Get(t.Context(), "u"); err == nil {
			t.Fatal("invalid version accepted")
		}
	}
	peer.err = errors.New("secret peer error")
	if _, err := c.Get(t.Context(), "u"); err == nil || err.Error() == peer.err.Error() {
		t.Fatal("unsanitized peer error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Get(ctx, "u"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := NewDataVersions(nil).Get(t.Context(), "u"); err == nil {
		t.Fatal("unconfigured peer ignored")
	}
}

func TestUnitVersionOutageClassification(t *testing.T) {
	detailed, _ := status.New(codes.Unavailable, "private").WithDetails(&errdetails.ErrorInfo{Reason: "PRIVATE"})
	for _, tc := range []struct {
		err       error
		transient bool
	}{
		{status.Error(codes.Unavailable, "private"), true}, {status.Error(codes.DeadlineExceeded, "private"), true},
		{detailed.Err(), false}, {finerr.InvalidToken(), false}, {finerr.TokenRevoked(), false}, {status.Error(codes.PermissionDenied, "private"), false}, {status.Error(codes.Canceled, "private"), false}, {errors.New("private"), false},
	} {
		peer := &versionClient{err: tc.err}
		_, err := NewDataVersions(peer).Get(t.Context(), "u")
		if err == nil || errors.Is(err, dialogue.ErrVersionUnavailable) != tc.transient {
			t.Fatal(tc.err, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := NewDataVersions(peer).Get(ctx, "u"); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
}
