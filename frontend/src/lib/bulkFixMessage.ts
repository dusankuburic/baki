export type BulkFixMessage = {variant: 'success' | 'warning'; title: string; description: string}

export function bulkFixMessage(applied: number, ruleCount: number, partial: boolean): BulkFixMessage {
  if (partial) {
    return {
      variant: 'warning',
      title: 'Applied fixes before the time budget ran out',
      description: `${applied} fix(es) across ${ruleCount} rule(s) were saved; run Fix again to continue.`,
    }
  }
  return {variant: 'success', title: 'Applied fixes', description: `${applied} fix(es) across ${ruleCount} rule(s).`}
}
