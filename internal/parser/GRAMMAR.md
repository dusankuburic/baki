# PAD Export Grammar

Documented from observing real Power Automate Desktop "Copy actions" output
and the PAD designer's clipboard format (text representation).

## 1. Top-level structure

A PAD export is a sequence of **subflow** blocks. Subflow headers and end markers
appear at indent 0.

Two subflow formats are observed:

**Region style (older):**
```
# Region "SubflowName"
    [actions]
# End Region
```

**Dotted style (newer):**
```
Display.ForEachSubflow
    [body lines]
Display.End
```

**Key observations:**
- Subflow bodies are indented by multiples of `    ` (4 spaces)
- The last subflow's end marker is followed by nothing (end of file)

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

**Dual format support:** PAD exports come in two styles, both of which the parser handles:
1. **Keyword style**: `IF`, `ELSE`, `END`, `LOOP`, `COMMENT`, `SET`
2. **Dotted style**: `Display.If`, `Loop.ForEach`, `Variables.SetVariable`, `Text.If`

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

Keyword-style equivalent: `LOOP FROM ...`, `LOOP WHILE ...`, `LOOP FOREACH ...`

### 3.2 Conditionals

Dotted style:
```
Text.If          Condition: $'''%x% == %y%'''
    [true branch]
Text.Else
    [false branch]
Text.EndIf
```

Keyword style:
```
IF %Status% = "Success"
    [actions]
ELSE
    [actions]
END
```

### 3.3 On Block Error

```
ON BLOCK ERROR
    [handler actions]
END
```

### 3.4 Comments

```
COMMENT  This is a comment
```

### 3.5 Variable Set

Keyword style: `SET VariableName TO Value`
Dotted style: `Variables.SetVariable Value: ... => VariableName`

## 4. Variable references

Variables are always wrapped in `%...%`:
```
%VariableName%
%ListVar[1]%
%DataRow['ColumnName']%
```

## 5. Property value quoting

- Simple values: unquoted, space-delimited until next `Key:` or `=>`
- Quoted strings: `'''text'''`, `$'''text with %vars% and newlines'''`
- Enum values: `Namespace.Enum.Value` (dot notation, no quotes)

## 6. Output variables

Trailing `=> VarName` captures the action's output:
```
Display.ShowMessageBox Message: "Hello" => ButtonPressed
```

## 7. Indent rules

- Each nesting level adds 4 spaces
- Top-level actions inside a subflow are at indent 0 (relative to subflow)
- Subflow headers are at the outermost level (no indent)
- Some exports use 0-based indent for the first subflow level
- Others indent subflow body by 4 spaces

## 8. Empty lines

Empty lines are preserved in the export and carry no semantic meaning.
They should be tracked for line numbering but produce `TokEmpty` tokens.

## 9. Special cases

- Trailing whitespace on lines
- BOM at start of file (UTF-8 BOM)
- CRLF or LF line endings
- Mixed indent (tabs + spaces) — treat tabs as 4 spaces
- Very long lines (>1000 chars) for complex property values
- Unicode in string values
