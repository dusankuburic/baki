// Default-only re-exports, matching the convention used by the other component
// barrels (chat/, findings/, flow/, etc.). CollapsibleSection and HistoryTab
// use named exports and are imported by their direct path where needed.
export {default as InspectorTabs} from './InspectorTabs'
export {default as DetailsTab} from './DetailsTab'
export {default as DetailsHeader} from './DetailsHeader'
export {default as PropertiesTable} from './PropertiesTable'
export {default as VariableChips} from './VariableChips'
export {default as BlockMetadata} from './BlockMetadata'
export {default as ChildrenList} from './ChildrenList'
export {default as MetricsTab} from './MetricsTab'
