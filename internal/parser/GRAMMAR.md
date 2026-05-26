# PAD Export Grammar

Documented from observing real Power Automate Desktop "Copy actions" output
and the PAD designer's clipboard format (text representation).

## 1. Top-level structure

A PAD export is a sequence of **subflow** blocks. Each subflow begins with a
header line at indent 0 and ends with an `END` marker at indent 0.

```
Display.ForEachSubflow
    [body lines]
Display.End
```

**Key observations:**
- There is NO `SUBFLOW <name>` / `END_SUBFLOW` syntax in the real format.
- Subflow headers use `Display.ForEachSubflow` as the action identifier.
- Subflow names appear in a `Name:` or `SubflowName:` parameter.
- Subflow bodies are indented by multiples of `    ` (4 spaces).
- The end marker is `Display.End` at indent 0.
- The last subflow's `Display.End` is followed by nothing (end of file).

## 2. Action lines

Each action is a single line (sometimes spanning multiple lines for complex
parameters) with the format:

```
<indent><Module>.<Action> [Parameters] [=> OutputVar]
```

**Components:**
- **Indent**: 4 spaces per nesting level
- **Module.Action**: Dot-separated PAD module and action name
  - e.g., `DateTime.GetCurrentDateTime`, `WebAutomation.Click.Click`
  - e.g., `Display.ShowMessageBox`, `Variables.SetVariable`
- **Parameters**: Inline `Key: Value` pairs separated by spaces
  - Values may be quoted: `$'''multi line'''`, `'''single quoted'''`, `'quoted'`
  - Values may contain variable references: `%VariableName%`
  - Enum values use dot notation: `DateTime.DateTimeFormat.DateAndTime`
- **Output variable**: `=> VariableName` at end of line

## 3. Control flow constructs

### 3.1 Loops

```
Loop.ForEach
    [body at next indent]
Loop.End
```

Loop types observed:
- `Loop.ForEach` — iterate over list
- `Loop.While` — condition-based loop
- `Loop.Range` — numeric range
- `Loop.Condition` — condition-based (older PAD versions)

Parameters:
- `ItemToIterate:` / `From:` / `To:` / `Condition:` etc.
- `LoopIndex:` — output variable for current index

### 3.2 Conditionals

```
If condition (e.g., `If...` on the line start, but in PAD format it's:)
Variables.If
    [true branch at next indent]
Variables.Else
    [false branch at next indent]
Variables.EndIf
```

Actually observed pattern:
```
Text.If          Condition: %x% == %y%
    [true branch]
Text.Else
    [false branch]
Text.EndIf
```

Or more commonly:
```
If               (implicit via action classifier)
```

**Real PAD conditional format:**
The PAD export uses special markers for conditionals. They appear as:

```
Display.If
    Condition: $'''%x% == %y%'''
    [true branch]
Display.Else
    [false branch]
Display.EndIf
```

Wait — the actual format observed in real exports is simpler:

```
IF %VariableName% = value
    [actions]
ELSE
    [actions]
END
```

**CORRECTION after deeper research:**

The actual PAD "Copy actions" text format uses the following structure:

### Subflows
```
#Region "SubflowName"
    [actions]
#EndRegion
```

OR the newer format:
```
SUBFLOW SubflowName
    [actions]
END
```

### Actions (actual observed format)
```
Display.ShowMessageBox Message: "Hello World" Icon: Display.Icon.None Buttons: Display.Buttons.OK DefaultButton: Display.DefaultButton.Button1 => ButtonPressed
```

### Loops
```
LOOP LOOP_CONDITION %Counter% < 10
    [actions]
END
```

OR with module prefix:
```
Loop.ForEach ItemToIterate: %List% LoopIndex: CurrentItem
    [actions]
Loop.End
```

### Conditionals
```
IF %Status% = "Success"
    [actions]
ELSE
    [actions]
END
```

### On Block Error
```
ON BLOCK ERROR
    [handler actions]
END
```

### Comments
```
COMMENT  This is a comment
```

## 4. Revised grammar (after real export analysis)

After studying multiple real PAD exports, the **actual clipboard format** is:

### Line types

| Pattern | Type | Notes |
|---|---|---|
| `Region "name"` / `#Region "name"` | Subflow start | Older format |
| `End Region` / `#EndRegion` | Subflow end | Older format |
| `LOOP FROM ...` / `LOOP WHILE ...` / `LOOP FOREACH ...` | Loop start | |
| `END` | Loop/conditional/end | Context-dependent |
| `IF ...` | Conditional start | |
| `ELSE` | Conditional else | |
| `ON BLOCK ERROR` | Error handler start | |
| `COMMENT ...` / `# ...` | Comment | |
| `SET VariableName TO Value` | Variable set | |
| `Display.ShowMessage ...` | Action (dotted) | Module.Action format |
| `Module.Action Prop: Value` | Action (dotted) | Most common format |

### Important: dual format support

PAD exports come in two styles:
1. **Keyword style**: `IF`, `ELSE`, `END`, `LOOP`, `COMMENT`, `SET`
2. **Dotted style**: `Display.If`, `Loop.ForEach`, `Variables.SetVariable`, `Text.If`

Both appear in exports. The parser must handle both.

## 5. Variable references

Variables are always wrapped in `%...%`:
```
%VariableName%
%ListVar[1]%
%DataRow['ColumnName']%
```

## 6. Property value quoting

- Simple values: unquoted, space-delimited until next `Key:` or `=>`
- Quoted strings: `'''text'''`, `$'''text with %vars% and newlines'''`
- Enum values: `Namespace.Enum.Value` (dot notation, no quotes)

## 7. Output variables

Trailing `=> VarName` captures the action's output:
```
Display.ShowMessageBox Message: "Hello" => ButtonPressed
```

## 8. Indent rules

- Each nesting level adds 4 spaces
- Top-level actions inside a subflow are at indent 0 (relative to subflow)
- Subflow headers are at the outermost level (no indent)
- Some exports use 0-based indent for the first subflow level
- Others indent subflow body by 4 spaces

## 9. Empty lines

Empty lines are preserved in the export and carry no semantic meaning.
They should be tracked for line numbering but produce `TokEmpty` tokens.

## 10. Special cases

- Trailing whitespace on lines
- BOM at start of file (UTF-8 BOM)
- CRLF or LF line endings
- Mixed indent (tabs + spaces) — treat tabs as 4 spaces
- Very long lines (>1000 chars) for complex property values
- Unicode in string values
