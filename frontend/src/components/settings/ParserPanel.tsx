import Switch from '@/components/shared/Switch'
import {useSettingsStore} from '@/stores/settingsStore'
import Input from '@/components/shared/Input'

export default function ParserPanel() {
  const parser = useSettingsStore(s => s.settings.parser)
  const updateParser = useSettingsStore(s => s.updateParser)

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Parser</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Configure how Power Automate Desktop flows are parsed.
      </p>

      <div className="space-y-6">
        <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <div>
            <span className="text-sm font-medium text-text-primary">Preserve Comments</span>
            <p className="text-xs text-text-tertiary mt-0.5">
              Include comments in the parsed flow structure and AI context.
            </p>
          </div>
          <Switch
            checked={parser.preserveComments}
            onChange={(v) => updateParser({preserveComments: v})}
          />
        </div>

        <div className="space-y-4 pt-2">
          <div className="flex items-center justify-between py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
            <div>
              <span className="text-sm font-medium text-text-primary">Treat Tabs as Spaces</span>
              <p className="text-xs text-text-tertiary mt-0.5">
                Convert tab characters to spaces during parsing.
              </p>
            </div>
            <Switch
              checked={parser.treatTabsAsSpaces}
              onChange={(v) => updateParser({treatTabsAsSpaces: v})}
            />
          </div>

          {parser.treatTabsAsSpaces && (
            <div className="pl-4">
              <label className="text-xs font-medium text-text-secondary block mb-1.5">Spaces per Indent</label>
              <div className="w-24">
                <Input
                  type="number"
                  min={1}
                  max={8}
                  value={parser.spacesPerIndent}
                  onChange={(e) => updateParser({spacesPerIndent: parseInt(e.target.value) || 4})}
                />
              </div>
            </div>
          )}
        </div>

        <div className="pt-2">
          <label className="text-sm font-medium text-text-primary block mb-2">Maximum File Size (MB)</label>
          <div className="w-32">
            <Input
              type="number"
              min={1}
              max={500}
              value={parser.maxFileSizeMB}
              onChange={(e) => updateParser({maxFileSizeMB: parseInt(e.target.value) || 50})}
            />
          </div>
          <p className="text-xs text-text-tertiary mt-2">
            Larger flows require more memory and processing time.
          </p>
        </div>
      </div>
    </div>
  )
}
