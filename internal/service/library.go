package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/logger"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// LibraryService manages CRUD operations for saved flows in cloud mode.
type LibraryService struct {
	storage   storageif.StorageBackend
	mode      config.DeploymentMode
	flowCache *FlowService // Needed to invalidate search index when flow is updated/deleted
}

func NewLibraryService(storage storageif.StorageBackend, flowCache *FlowService) *LibraryService {
	mode := config.ModeLocal
	if storage != nil {
		mode = config.ModeCloud
	}
	return &LibraryService{storage: storage, mode: mode, flowCache: flowCache}
}

// CanRead reports whether userID may read the flow.
func (s *LibraryService) CanRead(ctx context.Context, doc *storageif.FlowDocument, userID string) bool {
	if s.mode == config.ModeLocal {
		return true
	}
	if doc.OwnerID == "" || doc.OwnerID == userID {
		return true
	}
	if doc.OrganizationID != "" && s.storage != nil {
		// This ideally uses OrgService, but for now we'll check if the backend
		// supports org membership checks directly or just allow owners.
	}
	
	// Check collaborators
	if s.storage != nil {
		if collabs, err := s.storage.ListCollaborators(ctx, doc.ID); err == nil {
			for _, c := range collabs {
				if c.UserID == userID {
					return true
				}
			}
		}
	}

	return false
}

// ResolveOwnerName returns the email of the owner.
func (s *LibraryService) ResolveOwnerName(ctx context.Context, ownerID string) string {
	if ownerID == "" || s.storage == nil {
		return ""
	}
	if u, err := s.storage.LoadUserByID(ctx, ownerID); err == nil {
		return u.Email
	}
	return ""
}

// ResolveOwnerNames resolves many owner IDs to display names (emails) in a
// single backend round trip, avoiding the N+1 pattern when building list
// responses. Unknown/empty IDs are simply absent from the returned map.
func (s *LibraryService) ResolveOwnerNames(ctx context.Context, ownerIDs []string) map[string]string {
	names := make(map[string]string, len(ownerIDs))
	if s.storage == nil || len(ownerIDs) == 0 {
		return names
	}
	users, err := s.storage.LoadUsersByIDs(ctx, ownerIDs)
	if err != nil {
		return names
	}
	for id, u := range users {
		names[id] = u.Email
	}
	return names
}

// ListLibraryFlows returns flows visible to the requesting user.
func (s *LibraryService) ListLibraryFlows(ctx context.Context, userID, orgID, query string, limit, offset int) (docs []*storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.ListLibraryFlows", &err)
	if s.mode == config.ModeLocal {
		return []*storageif.FlowDocument{}, nil
	}
	return s.storage.ListFlows(ctx, storageif.FlowFilter{
		UserID:         userID,
		OrganizationID: orgID,
		Query:          query,
		Limit:          limit,
		Offset:         offset,
		MetadataOnly:   true, // list view needs metadata only, not full content
	})
}

// GetLibraryFlow loads a single flow by ID without an access check.
// Prefer GetLibraryFlowForUser when the caller needs read enforcement.
func (s *LibraryService) GetLibraryFlow(ctx context.Context, flowID string) (doc *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.GetLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return nil, storageif.ErrNotFound
	}
	return s.storage.LoadFlow(ctx, flowID)
}

// GetLibraryFlowForUser loads a flow and returns ErrPermissionDenied if userID
// does not have read access. Use this in handlers instead of calling GetLibraryFlow
// followed by a separate CanRead check.
func (s *LibraryService) GetLibraryFlowForUser(ctx context.Context, flowID, userID string) (doc *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.GetLibraryFlowForUser", &err)
	doc, err = s.GetLibraryFlow(ctx, flowID)
	if err != nil {
		return nil, err
	}
	if !s.CanRead(ctx, doc, userID) {
		return nil, ErrPermissionDenied
	}
	return doc, nil
}

// CreateLibraryFlow persists a new flow owned by ownerID.
func (s *LibraryService) CreateLibraryFlow(ctx context.Context, ownerID, orgID string, doc storageif.FlowDocument) (saved *storageif.FlowDocument, err error) {
	defer logger.Guard("LibraryService.CreateLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return nil, fmt.Errorf("cloud storage not available in local mode")
	}

	if doc.ID == "" {
		doc.ID = uuid.NewString()
	}

	doc.OwnerID = ownerID
	doc.OrganizationID = orgID
	if err := s.storage.SaveFlow(ctx, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// UpdateLibraryFlow saves changes to an existing flow. Returns ErrPermissionDenied
// if callerID is not the flow owner (in cloud mode). Flows with an empty OwnerID
// are considered unclaimed and are not world-writable.
func (s *LibraryService) UpdateLibraryFlow(ctx context.Context, doc *storageif.FlowDocument, callerID string) (err error) {
	defer logger.Guard("LibraryService.UpdateLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return fmt.Errorf("cloud storage not available in local mode")
	}
	if doc.OwnerID == "" || doc.OwnerID != callerID {
		return ErrPermissionDenied
	}
	if err := s.storage.SaveFlow(ctx, doc); err != nil {
		return err
	}
	// Content changed in place (same ID) — drop any cached search index
	if s.flowCache != nil {
		s.flowCache.InvalidateSearchIndex(doc.ID)
	}
	return nil
}

// DeleteLibraryFlow removes a flow. Returns ErrPermissionDenied if callerID is
// not the flow owner (in cloud mode). Returns ErrNotFound in local mode.
func (s *LibraryService) DeleteLibraryFlow(ctx context.Context, flowID, callerID string) (err error) {
	defer logger.Guard("LibraryService.DeleteLibraryFlow", &err)
	if s.mode == config.ModeLocal {
		return ErrNotFound
	}
	doc, err := s.storage.LoadFlow(ctx, flowID)
	if err != nil {
		return err
	}
	if doc.OwnerID == "" || doc.OwnerID != callerID {
		return ErrPermissionDenied
	}
	if err := s.storage.DeleteFlow(ctx, flowID); err != nil {
		return err
	}
	if s.flowCache != nil {
		s.flowCache.InvalidateSearchIndex(flowID)
	}
	return nil
}
