package client

import (
	"context"
	"errors"

	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/dialogue"
)

// DataVersions adapts the system data-version RPC to the dialogue's consumer interface.
type DataVersions struct {
	client statementv1.AssistantToolsServiceClient
}

// NewDataVersions accepts nil when the peer is not configured. A source-bearing read
// then fails rather than claiming no changes in data it could not inspect.
func NewDataVersions(c statementv1.AssistantToolsServiceClient) *DataVersions {
	return &DataVersions{client: c}
}

// Get reads one version without forwarding the user's access token.
func (c *DataVersions) Get(ctx context.Context, user string) (dialogue.Version, error) {
	if c.client == nil {
		return dialogue.Version{}, errors.New("statement data peer is not configured")
	}
	response, err := c.client.GetDataVersion(ctx, &statementv1.GetDataVersionRequest{UserId: user})
	if err != nil {
		if ctx.Err() != nil {
			return dialogue.Version{}, ctx.Err()
		}
		if st, ok := status.FromError(err); ok && len(st.Proto().Details) == 0 && (st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded) {
			return dialogue.Version{}, dialogue.ErrVersionUnavailable
		}
		return dialogue.Version{}, errors.New("statement data version unavailable")
	}
	v := response.GetVersion()
	revisions, err := financialRevisions(v.GetRevisions())
	if err != nil {
		return dialogue.Version{}, errors.New("invalid data version")
	}
	return dialogue.Version{Revisions: revisions}, nil
}
