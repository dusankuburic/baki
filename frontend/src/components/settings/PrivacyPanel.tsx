// Downloading account data and deleting the account live on the Profile page
// (AccountDataCard) now, so account management isn't split across two panes.
// This panel stays purely informational.
export default function PrivacyPanel() {
  return (
    <div>
      <h2 className="text-xl font-semibold text-text-primary">Privacy</h2>
      <p className="text-sm text-text-secondary mt-1 mb-6">Control how PAD Analyzer handles your data.</p>

      <div className="space-y-4">
        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <span className="text-sm font-medium text-text-primary">API Keys</span>
          <p className="text-xs text-text-tertiary mt-0.5">
            All API keys are stored securely in your operating system's keychain. They are never sent to any server
            other than the respective AI provider.
          </p>
        </div>

        <div className="py-3 px-4 rounded-lg bg-surface-2 border border-border-default">
          <span className="text-sm font-medium text-text-primary">Data sent to AI providers</span>
          <p className="text-xs text-text-tertiary mt-0.5">
            Before any flow content is sent to an AI provider, known credential fields, secret-shaped patterns (tokens,
            connection strings), and high-entropy strings are automatically redacted.
          </p>
        </div>
      </div>
    </div>
  )
}
