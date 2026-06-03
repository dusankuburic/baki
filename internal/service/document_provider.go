package service

import (
	"context"
	"encoding/json"
	"fmt"

	"pad-analyzer/internal/models"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// DocumentProvider abstracts the difference between single-user (local)
// and multi-user (cloud) flow resolution.
type DocumentProvider interface {
	ResolveDoc(ctx context.Context, id string) (*models.FlowDocument, error)
	SetCurrentDoc(doc *models.FlowDocument)
	CurrentDoc() *models.FlowDocument
}

// LocalDocumentProvider holds a single current flow in memory.
type LocalDocumentProvider struct {
	doc *models.FlowDocument
}

func NewLocalDocumentProvider() *LocalDocumentProvider {
	return &LocalDocumentProvider{}
}

func (p *LocalDocumentProvider) ResolveDoc(ctx context.Context, id string) (*models.FlowDocument, error) {
	if p.doc == nil {
		return nil, ErrUninitialized
	}
	return p.doc, nil
}

func (p *LocalDocumentProvider) SetCurrentDoc(doc *models.FlowDocument) {
	p.doc = doc
}

func (p *LocalDocumentProvider) CurrentDoc() *models.FlowDocument {
	return p.doc
}

// CloudDocumentProvider loads flows from a shared storage backend.
type CloudDocumentProvider struct {
	storage storageif.StorageBackend
}

func NewCloudDocumentProvider(storage storageif.StorageBackend) *CloudDocumentProvider {
	return &CloudDocumentProvider{storage: storage}
}

func (p *CloudDocumentProvider) ResolveDoc(ctx context.Context, id string) (*models.FlowDocument, error) {
	if p.storage == nil {
		return nil, fmt.Errorf("cloud storage not configured")
	}
	libDoc, err := p.storage.LoadFlow(ctx, id)
	if err != nil {
		return nil, err
	}
	var doc models.FlowDocument
	if err := json.Unmarshal(libDoc.Content, &doc); err != nil {
		return nil, fmt.Errorf("invalid flow content: %w", err)
	}
	doc.OwnerID = libDoc.OwnerID
	doc.OrganizationID = libDoc.OrganizationID
	doc.RebuildIndexes()
	return &doc, nil
}

func (p *CloudDocumentProvider) SetCurrentDoc(doc *models.FlowDocument) {
	// No-op in cloud mode
}

func (p *CloudDocumentProvider) CurrentDoc() *models.FlowDocument {
	// No concept of "current" doc across requests in cloud mode
	return nil
}
