export default function PrivacyPanel() {
  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Privacy</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">
        Control how PAD Analyzer handles your data.
      </p>

      <div className="space-y-4">
        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <span className="text-sm font-medium text-text-primary">API Keys</span>
          <p className="text-xs text-text-tertiary mt-0.5">
            All API keys are stored securely in your operating system's keychain.
            They are never sent to any server other than the respective AI provider.
          </p>
        </div>
      </div>
    </div>
  )
}
