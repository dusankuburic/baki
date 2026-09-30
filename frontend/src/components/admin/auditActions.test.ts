import {describe, it, expect} from 'vitest'
import {AUDIT_GROUPS, actionGroup, actionTone, groupActions} from './auditActions'
import {actionLabel} from '@/components/dashboard/home/activity'
import {enDashboard} from '@/i18n/en.dashboard'
import {enAdmin} from '@/i18n/en.admin'

describe('auditActions', () => {
  it('groups actions by their prefix, dotless org ids included, and anything unknown under other', () => {
    expect(actionGroup('auth.login')).toBe('auth')
    expect(actionGroup('org_channel_save')).toBe('org')
    expect(actionGroup('azure.connection_save')).toBe('azure')
    expect(actionGroup('azure.model_delete')).toBe('azure')
    expect(actionGroup('mystery.thing')).toBe('other')
    expect(actionGroup('other.x')).toBe('other')
  })

  it('files the Azure AI Foundry actions in their own group, in display order', () => {
    const groups = groupActions([
      'azure.model_save',
      'org.update',
      'azure.connection_delete',
      'keys.save',
      'azure.connection_save',
      'azure.model_delete',
      'weird',
    ])
    expect(groups.map(g => g.group)).toEqual(['keys', 'azure', 'org', 'other'])
    expect(groups.find(g => g.group === 'azure')?.actions).toEqual([
      'azure.connection_delete',
      'azure.connection_save',
      'azure.model_delete',
      'azure.model_save',
    ])
  })

  it('labels every group and the Azure AI Foundry actions', () => {
    for (const group of AUDIT_GROUPS) expect(enAdmin.audit.groups[group]).toBeTruthy()
    expect(actionLabel(enDashboard.actions, 'azure.connection_save')).toBe('Saved an Azure AI Foundry resource')
    expect(actionLabel(enDashboard.actions, 'azure.connection_delete')).toBe('Removed an Azure AI Foundry resource')
    expect(actionLabel(enDashboard.actions, 'azure.model_save')).toBe('Saved an Azure AI Foundry model')
    expect(actionLabel(enDashboard.actions, 'azure.model_delete')).toBe('Removed an Azure AI Foundry model')
  })

  it('tints failed sign-ins and administration, and leaves the rest neutral', () => {
    expect(actionTone('auth.login_failure')).toContain('semantic-error')
    expect(actionTone('admin.role_change')).toContain('brand')
    expect(actionTone('azure.model_save')).toBe('bg-surface-3 text-text-secondary')
  })
})
