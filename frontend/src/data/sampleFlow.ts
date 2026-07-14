// A bundled sample PAD flow for first-run onboarding. Realistic enough to feel
// real (a "Customer Data Sync" flow) and deliberately seeded with a handful of
// common issues so a brand-new user sees findings immediately — proving the
// product's value without having to export their own flow first.
//
// Seeded findings (so the demo is instructive):
//   - hardcoded-credential  : an AWS-key-shaped value in ApiKey
//   - unused-variable       : DebugFlag is set but never read
//   - unhandled-error       : HTTPClient.InvokeUrl with no On Block Error
//   - missing-timeout       : HTTPClient.InvokeUrl with no timeout
//   - slow-pattern          : a UI click inside a LOOP with no wait
//   - hardcoded-filepath    : an absolute Windows path
//
// The text uses the PAD export grammar the parser already handles (keyword
// LOOP/IF/END + Region markers + Module.Action params), so it round-trips the
// same way a real Power Automate Desktop "Copy actions" export does.

export const SAMPLE_FLOW_NAME = 'Customer Data Sync (sample)'

export const SAMPLE_FLOW_FILES: Record<string, string> = {
  'Main.txt': `#Region "Main"
Variables.SetVariable Name: %ApiKey% Value: 'AKIAIOSFODNN7EXAMPLE'
Variables.SetVariable Name: %DebugFlag% Value: True
HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/customers' Accept: 'application/json' => %Response%
LOOP FROM 1 TO 50 STEP 1
    WebAutomation.ClickLink BrowserInstance: %Browser% Link: 'next page'
    IF %Response% = '' THEN
        Display.ShowMessageBox Message: 'No data returned from API'
    END
END
Text.WriteToFile TextToWrite: %Response% FilePath: 'C:\\Reports\\customers.txt' IfFileExists: Overwrite
#EndRegion
`,
}
