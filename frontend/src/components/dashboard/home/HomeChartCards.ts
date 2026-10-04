// The Home cards that draw with recharts, behind ONE dynamic import: Home
// (HomeDashboard) lazy-loads them through this module so its own chunk paints
// the chart-free content — header, KPI row, recent flows, activity, security
// — while the charts chunk is still in flight. One module (not seven
// `import()`s) keeps the cards together in a single chunk, and Vite dedupes
// the promise for every `lazy()` that resolves through it.
//
// Keep this list in step with the cards that import from `@/components/charts`
// beyond ChartCard/StatTile: a card added here must not be imported
// statically from HomeDashboard, and a chart-free card does not belong here.
export {HealthTrendCard} from './HealthTrendCard'
export {FindingsByCategoryCard} from './FindingsByCategoryCard'
export {FindingsByRuleCard} from './FindingsByRuleCard'
export {TriageCard} from './TriageCard'
export {AiUsageCard} from './AiUsageCard'
export {HealthDistributionCard} from './HealthDistributionCard'
export {FlowComplexityCard} from './FlowComplexityCard'
