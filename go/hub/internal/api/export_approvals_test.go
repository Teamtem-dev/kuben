package api

import (
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// ApprovalDtoOf exposes approvalDto with the actors given as user id →
// email.
func ApprovalDtoOf(runID uuid.UUID, a store.RunApproval, names map[string]string, canDecide bool) *gen.ApprovalDto {
	return approvalDto(runID, a, actors(names), canDecide)
}
