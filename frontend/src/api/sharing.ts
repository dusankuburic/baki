import { request } from './client'

export type Permission = 'viewer' | 'editor' | 'admin'

export interface Collaborator {
  userId: string
  email: string
  displayName?: string
  avatarUrl?: string
  permission: Permission
  grantedAt: string
}

export interface ShareFlowRequest {
  flowId: string
  userId?: string
  email?: string
  permission: Permission
}

export interface UpdatePermissionRequest {
  flowId: string
  userId: string
  permission: Permission
}

export const sharingApi = {
  listCollaborators: (flowId: string): Promise<Collaborator[]> =>
    request(`/api/flows/${flowId}/collaborators`, undefined, 'GET'),

  addCollaborator: (req: ShareFlowRequest): Promise<Collaborator> =>
    request(`/api/flows/${req.flowId}/collaborators`, req),

  updatePermission: (req: UpdatePermissionRequest): Promise<Collaborator> =>
    request(`/api/flows/${req.flowId}/collaborators/${req.userId}`, req, 'PUT'),

  removeCollaborator: (flowId: string, userId: string): Promise<void> =>
    request(`/api/flows/${flowId}/collaborators/${userId}`, undefined, 'DELETE'),
}
