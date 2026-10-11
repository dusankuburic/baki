import {describe, it, expect, expectTypeOf} from 'vitest'
import type {z} from 'zod'
import {RULE_STAGES, isKnown, type RuleStage, type Tolerant} from '@/types'
import {buildSchemas} from './schemas.lazy'

const s = buildSchemas()

const finding = (over: Record<string, unknown> = {}) => ({
  id: 'F-001',
  ruleId: 'insecure-http-url',
  severity: 'warning',
  title: 'Insecure URL',
  description: 'http://',
  ...over,
})

describe('Finding.stage', () => {
  it('keeps preview, an absent stage and a stage this bundle does not know', () => {
    const out = s.AnalysisReportSchema.parse({
      flowId: 'f',
      generatedAt: '2026-10-08T00:00:00Z',
      durationMs: 1,
      findings: [finding({stage: 'preview'}), finding({id: 'F-002'}), finding({id: 'F-003', stage: 'beta'})],
      stats: {errors: 0, warnings: 3, info: 0, blocksAnalyzed: 1, rulesRun: 45},
    })
    expect(out.findings.map(f => f.stage)).toEqual(['preview', undefined, 'beta'])
    expect(isKnown(RULE_STAGES, out.findings[2]!.stage)).toBe(false)
    type Stage = z.output<typeof s.FindingSchema>['stage']
    expectTypeOf<Stage>().toEqualTypeOf<Tolerant<RuleStage> | undefined>()
  })

  it('still rejects a stage that is not a string', () => {
    expect(s.FindingSchema.safeParse(finding({stage: 2})).success).toBe(false)
  })
})
