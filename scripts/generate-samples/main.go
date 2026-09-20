package main

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
)

func main() {
	var sb strings.Builder
	actions := []struct {
		format string
		vars   []string
	}{
		{"Display.ShowMessageBox Message: $'''Message %d''' Icon: Display.Icon.None => ButtonPressed", nil},
		{"Variables.SetVariable NewValue: $'''Value %d''' Name: Var%d", nil},
		{"Variables.IncreaseVariable Value: 1 Name: Counter", nil},
		{"DateTime.GetCurrentDateTime DateTimeFormat: DateTime.DateTimeFormat.DateAndTime => CurrentDateTime", nil},
		{"WebAutomation.Click.Click BrowserInstance: %Browser% Control: appmask['Button%d']", []string{"Browser"}},
		{"WebAutomation.NavigateTo Url: $'''https://example.com/page%d''' BrowserInstance: %Browser%", []string{"Browser"}},
		{"Text.Replace TextToSearch: %InputText% TextToFind: $'''old''' IsRegEx: False IgnoreCase: True => Result", []string{"InputText"}},
		{"File.WriteText File: $'''C:\\temp\\output_%d.txt''' TextToWrite: $'''Content''' Encoding: File.TextEncoding.UTF8", nil},
		{"Excel.ReadFromExcelExcelFileToRead: CurrentExcelFile Instance: %ExcelInstance% ReadOption: Excel.ReadOption.CellRange Range: $'''A1:B10''' => Data", []string{"ExcelInstance"}},
		{"Folder.GetSpecialFolder SpecialFolder: Folder.SpecialFolder.Desktop => DesktopPath", nil},
		{"System.RunApplication ApplicationPath: $'''notepad.exe''' WindowStyle: System.WindowStyle.Maximized", nil},
		{"Mouse.MoveMouse X: %d Y: %d MovementType: Mouse.MovementType.Absolute", nil},
		{"Keyboard.PressKeys Keys: $'''{Enter}'''", nil},
		{"Email.SendEmail Server: $'''smtp.example.com''' Port: 587 From: $'''bot@example.com''' To: $'''user%d@example.com''' Subject: $'''Report %d''' Body: $'''Hello, report attached.'''", nil},
	}

	subflows := []string{"Main", "ProcessData", "HandleErrors", "GenerateReport", "Cleanup", "Validation", "Export", "Logging"}

	linesPerSubflow := 1250
	totalSubflows := 8

	for s := 0; s < totalSubflows; s++ {
		fmt.Fprintf(&sb, "#Region \"%s\"\n", subflows[s])
		sb.WriteString("    COMMENT  Auto-generated subflow for benchmarking\n")

		i := 0
		for i < linesPerSubflow {
			if i > 0 && i%50 == 0 && i+10 < linesPerSubflow {
				sb.WriteString("    LOOP FOREACH CurrentItem IN %ItemList%\n")
				sb.WriteString("        IF %CurrentItem% > 0\n")
				for j := 0; j < 5; j++ {
					a := actions[rand.Intn(len(actions))]
					sb.WriteString("            ")
					fmt.Fprintf(&sb, a.format, rand.Intn(10000), rand.Intn(100))
					sb.WriteString("\n")
				}
				sb.WriteString("        ELSE\n")
				for j := 0; j < 3; j++ {
					a := actions[rand.Intn(len(actions))]
					sb.WriteString("            ")
					fmt.Fprintf(&sb, a.format, rand.Intn(10000), rand.Intn(100))
					sb.WriteString("\n")
				}
				sb.WriteString("        END\n")
				sb.WriteString("    END\n")
				i += 12
				continue
			}

			if i > 0 && i%200 == 0 {
				sb.WriteString("    ON BLOCK ERROR\n")
				sb.WriteString("        Display.ShowMessageBox Message: $'''Error occurred'''\n")
				sb.WriteString("        SET ErrorHandled TO True\n")
				sb.WriteString("    END\n")
				i += 4
				continue
			}

			a := actions[rand.Intn(len(actions))]
			sb.WriteString("    ")
			fmt.Fprintf(&sb, a.format, rand.Intn(10000), rand.Intn(100))
			sb.WriteString("\n")
			i++
		}

		sb.WriteString("#EndRegion\n")
	}

	output := sb.String()
	fmt.Fprintf(os.Stderr, "Generated %d lines\n", strings.Count(output, "\n"))
	// Default target is the parser's sample corpus (the parser moved from
	// internal/ to core/; the old path silently wrote into a directory that
	// no longer exists and the error was discarded). Run from the repo root:
	// `npm run gen:samples`, or pass an explicit output path.
	out := "core/parser/testdata/samples/synthetic_10k.txt"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := os.WriteFile(out, []byte(output), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "generate-samples: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Wrote %s\n", out)
}
