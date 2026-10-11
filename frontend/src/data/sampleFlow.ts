
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
