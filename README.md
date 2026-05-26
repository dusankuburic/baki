# PAD Analyzer

Desktop app for parsing, visualizing, and analyzing Power Automate Desktop flow exports.

Built with **Tauri v2**, **Go** (Sidecar), **React**, **TypeScript**, and **Tailwind CSS**.

## Prerequisites

- [Go](https://go.dev/dl/) 1.23+
- [Node.js](https://nodejs.org/) 20+
- [Rust](https://www.rust-lang.org/tools/install) (for Tauri host)

## Development

1. **Install Dependencies**:
   ```bash
   npm install
   cd frontend && npm install
   ```

2. **Start the Dev Server**:
   ```bash
   npm run dev
   ```

This builds the Go sidecar, starts the Vite dev server, and opens the Tauri window.

## Build

Production build:

```bash
npm run build
```

Output: `src-tauri/target/release/bundle/...`

## Tests

```bash
# Go tests (parser, analyzer, search, AI)
go test ./...

# Frontend type check
cd frontend && npx tsc --noEmit
```

## Loading Flows

The app supports two PAD export formats:

1. **Single file** — drop a `.txt` export into the app or use File > Open
2. **Folder** — load a folder containing `Main.txt` + subflow `.txt` files (e.g. `browsercloser/`, `padframework/`)

No `#Region` wrappers required — the parser creates an implicit subflow when they're missing.

## Keyboard Shortcuts

| Shortcut | Action |
|---|---|
| `Cmd/Ctrl + O` | Open file |
| `Cmd/Ctrl + ,` | Settings |
| `Cmd/Ctrl + K` | Command palette |
| `Cmd/Ctrl + Shift + A` | Run analysis |
| `Cmd/Ctrl + E` | Export PDF |
| `Cmd/Ctrl + Shift + E` | Export Markdown |
| `?` | Shortcuts help |
