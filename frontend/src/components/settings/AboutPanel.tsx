import {useEffect, useState} from 'react'
import {systemApi} from '@/api'
import type {AppInfo} from '@/types/domain'

export default function AboutPanel() {
  const [info, setInfo] = useState<AppInfo | null>(null)

  useEffect(() => {
    systemApi.appInfo().then((i: AppInfo) => setInfo({
      version: i?.version || 'dev',
      platform: i?.platform || '',
      arch: i?.arch || '',
      buildDate: i?.buildDate || '',
      gitCommit: i?.gitCommit || '',
    })).catch(() => setInfo({version: 'dev', platform: '', arch: '', buildDate: '', gitCommit: ''}))
  }, [])

  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">About PAD Analyzer</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Power Automate Desktop flow analysis tool.
      </p>

      <div className="space-y-4">
        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <div className="text-lg font-bold text-text-primary mb-2">PAD Analyzer</div>
          <div className="space-y-1 text-sm text-text-secondary">
            <div>Version: {info?.version ?? '...'}</div>
            <div>Platform: {info?.platform ?? ''} / {info?.arch ?? ''}</div>
          </div>
        </div>

        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default text-sm text-text-secondary">
          <p className="mb-2">
            PAD Analyzer helps you understand, analyze, and improve your
            Power Automate Desktop flows with static analysis rules and AI-powered insights.
          </p>
          <p className="text-xs text-text-tertiary">
            Built with Tauri, Go, React, and TypeScript.
          </p>
        </div>
      </div>
    </div>
  )
}
