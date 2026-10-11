import {useTranslation} from 'react-i18next'
import {CollapsibleSection} from './CollapsibleSection'
import clsx from 'clsx'

type PropertiesTableProps = {
  properties: Record<string, string>
}

export default function PropertiesTable({properties}: PropertiesTableProps) {
  const {t} = useTranslation('inspector')
  const entries = Object.entries(properties)
  if (entries.length === 0) return null

  const formatValue = (val: string) => {
    if (val.startsWith("$'''") && val.endsWith("'''")) {
      return {
        text: `"${val.slice(4, -3)}"`,
        isString: true,
      }
    }
    if (val.startsWith("'''") && val.endsWith("'''")) {
      return {
        text: `"${val.slice(3, -3)}"`,
        isString: true,
      }
    }
    if ((val.startsWith("'") && val.endsWith("'")) || (val.startsWith('"') && val.endsWith('"'))) {
      return {
        text: `"${val.slice(1, -1)}"`,
        isString: true,
      }
    }
    return {
      text: val,
      isString: false,
    }
  }

  return (
    <CollapsibleSection title={t('details.properties', {count: entries.length})}>
      <div className="space-y-2">
        {entries.map(([key, value]) => {
          const {text, isString} = formatValue(value)
          return (
            <div key={key} className="flex gap-2">
              <span className="text-xs text-text-tertiary font-medium w-2/5 shrink-0 truncate" title={key}>
                {key}
              </span>
              <span
                className={clsx(
                  'text-sm font-mono wrap-break-word flex-1',
                  isString ? 'text-block-string italic' : 'text-text-primary',
                )}
                title={value}
              >
                {text}
              </span>
            </div>
          )
        })}
      </div>
    </CollapsibleSection>
  )
}
