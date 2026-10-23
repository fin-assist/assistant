package client

import (
	"context"
	"errors"

	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"

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
		return dialogue.Version{}, errors.New("statement data version unavailable")
	}
	v := response.GetVersion()
	result := dialogue.Version{}
	if at := v.GetCategoriesChangedAt(); at != nil {
		if err := at.CheckValid(); err != nil {
			return dialogue.Version{}, errors.New("invalid data version")
		}
		t := at.AsTime()
		result.Categories = &t
	}
	if at := v.GetUploadsChangedAt(); at != nil {
		if err := at.CheckValid(); err != nil {
			return dialogue.Version{}, errors.New("invalid data version")
		}
		t := at.AsTime()
		result.Uploads = &t
	}
	return result, nil
}
