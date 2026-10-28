import {describe, it, expect} from 'vitest'
import i18n from '@/i18n'
import {actionLabel} from './activity'

describe('audit action labels', () => {
  const labels = () => i18n.t('dashboard:actions', {returnObjects: true}) as Record<string, string>

  it('names the knowledge identity stamp (R-knowledge-3) instead of spelling out its action id', () => {
    expect(labels().org_knowledge_identity_stamp).toBeTruthy()
    expect(actionLabel(labels(), 'org.knowledge_identity_stamp')).toBe(labels().org_knowledge_identity_stamp)
  })
})
