// Package account translates the account use cases to their gRPC contracts.
package account

import (
	"context"
	"strings"
	"time"
	_ "time/tzdata" // IANA validation must work in the distroless image without host zoneinfo.

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	deletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/reqctx"
)

// UseCases is the account behavior consumed by the handlers.
type UseCases interface {
	GetLimit(context.Context, string) (account.Limit, error)
	SetTimezone(context.Context, string, string, int32) error
	Purge(context.Context, string) error
}

// Handler implements the first account RPCs. Dialogue methods remain unimplemented
// until the subsequent FIN-220 stages; embedding never reports a fake success.
type Handler struct {
	assistantv1.UnimplementedAssistantServiceServer
	settingsv1.UnimplementedUserSettingsSyncServiceServer
	deletionv1.UnimplementedUserDeletionServiceServer
	uc UseCases
}

// Register attaches the three generated services to the service's guarded server.
func Register(s grpc.ServiceRegistrar, uc UseCases) {
	h := &Handler{uc: uc}
	assistantv1.RegisterAssistantServiceServer(s, h)
	settingsv1.RegisterUserSettingsSyncServiceServer(s, h)
	deletionv1.RegisterUserDeletionServiceServer(s, h)
}

// GetLimit uses only the verified user subject, never a request field.
func (h *Handler) GetLimit(ctx context.Context, _ *assistantv1.GetLimitRequest) (*assistantv1.GetLimitResponse, error) {
	id, ok := reqctx.UserID(ctx)
	if !ok {
		return nil, finerr.InvalidToken()
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, finerr.InvalidToken()
	}
	limit, err := h.uc.GetLimit(ctx, id)
	if err != nil {
		return nil, err
	}
	return &assistantv1.GetLimitResponse{Remaining: int32(limit.Remaining), DailyMax: int32(limit.DailyMax), ResetsAt: timestamppb.New(limit.ResetsAt), TimeZone: "Europe/Moscow"}, nil
}

// SetUserTimezone validates the system event before entering the write transaction.
func (h *Handler) SetUserTimezone(ctx context.Context, req *settingsv1.SetUserTimezoneRequest) (*settingsv1.SetUserTimezoneResponse, error) {
	id, err := userID(req.GetUserId())
	if err != nil {
		return nil, err
	}
	zone := req.GetTimezone()
	if zone == "" {
		return nil, violation("timezone", "REQUIRED")
	}
	// Local depends on the host. LoadLocation also accepts arbitrary paths only when
	// valid IANA names, but explicitly reject absolute paths and parent traversal.
	if zone == "Local" || strings.HasPrefix(zone, "/") || strings.Contains(zone, "..") {
		return nil, violation("timezone", "INVALID_FORMAT")
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return nil, violation("timezone", "INVALID_FORMAT")
	}
	if err := h.uc.SetTimezone(ctx, id, zone, req.GetSettingsVersion()); err != nil {
		return nil, err
	}
	return &settingsv1.SetUserTimezoneResponse{}, nil
}

// PurgeUserData acknowledges only a committed, durable deletion.
func (h *Handler) PurgeUserData(ctx context.Context, req *deletionv1.PurgeUserDataRequest) (*deletionv1.PurgeUserDataResponse, error) {
	id, err := userID(req.GetUserId())
	if err != nil {
		return nil, err
	}
	if err := h.uc.Purge(ctx, id); err != nil {
		return nil, err
	}
	return &deletionv1.PurgeUserDataResponse{}, nil
}

func userID(id string) (string, error) {
	if id == "" {
		return "", violation("user_id", "REQUIRED")
	}
	v, err := uuid.Parse(id)
	if err != nil {
		return "", violation("user_id", "INVALID_FORMAT")
	}
	return v.String(), nil
}

func violation(field, reason string) error {
	return finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: field, Reason: reason, Description: "Invalid field"})
}
