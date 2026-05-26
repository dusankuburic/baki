# PAD Analyzer

Desktop application for parsing, visualizing, and analyzing **Power Automate Desktop (PAD)** flow exports. Built with Tauri v2, Go, and React, it runs entirely offline and provides security auditing, variable tracking, execution graph visualization, AI-assisted analysis, and documentation export.

---

## Table of Contents

- [Features](#features)
- [Architecture Overview](#architecture-overview)
- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [Running in Development](#running-in-development)
- [Building for Production](#building-for-production)
- [Running Tests](#running-tests)
- [Project Structure](#project-structure)
- [AI Provider Configuration](#ai-provider-configuration)
- [Keyboard Shortcuts](#keyboard-shortcuts)
- [Troubleshooting](#troubleshooting)

---

## Features

### Flow Parsing
- **Single file**: Parse standalone `.txt` PAD exports.
- **Folder mode**: Load a full flow directory containing `Main.txt` and subflow `.txt` files.
- **Implicit subflows**: Automatically creates subflow boundaries when `#Region` wrappers are absent.

### AI-Powered Analysis
- **GitHub Copilot** — OAuth device flow or Personal Access Token (PAT).
- **GitHub Models** — GitHub's model marketplace.
- **Anthropic Claude**, **OpenAI GPT-4**, **Google Gemini**, **xAI Grok**, **Zhipu AI (GLM)**.
- All API keys are stored in the OS encrypted keyring — never written to disk in plaintext.

### Analysis Rules Engine
Static analysis with 10+ built-in rules:
- Hardcoded credential detection
- Dead code / unreachable branches
- Deep nesting warnings
- Duplicate action detection
- Empty error handler detection
- Infinite loop detection
- …and more, with severity levels (Error / Warning / Info)

### Visualization & Export
- **Variable lineage** — track variable lifecycle and transformations across subflows.
- **Execution graph** — DAG visualization of flow structure and call dependencies.
- **Export to PDF** or **Markdown** — full analysis reports with findings.

---

## Architecture Overview

```
┌──────────────────────────────────────────────────────────┐
│  Tauri v2 (Rust)  ←  OS window, file dialogs, OS keyring │
│                                                          │
│  ┌──────────────────────┐   ┌──────────────────────────┐ │
│  │  React / TypeScript  │   │  Go HTTP Sidecar         │ │
│  │  Vite + Tailwind CSS │◄──►  REST API + SSE          │ │
│  │  Zustand state       │   │  Parser, Analyzer,       │ │
│  │  Cytoscape.js graphs │   │  AI providers, Export    │ │
│  └──────────────────────┘   └──────────────────────────┘ │
└──────────────────────────────────────────────────────────┘
```

- **Tauri (Rust)** is the desktop host. It spawns the Go binary as a sidecar and manages native OS integration.
- **Go sidecar** runs an HTTP server on a random ephemeral port with an ephemeral bearer token for auth. It handles all heavy lifting: parsing, static analysis, AI calls, PDF/Markdown generation.
- **React frontend** communicates with the Go server over HTTP (commands) and SSE (real-time events such as streaming AI responses and analysis progress).
- **No external database** — all settings and secrets live in the OS keyring; recent files and preferences on the local filesystem.

---

## Prerequisites

Install the following before anything else.

### Go 1.23+
```bash
# Verify
go version
```
Download from https://go.dev/dl/

### Node.js 20+
```bash
# Verify
node --version
npm --version
```
Download from https://nodejs.org/ or use a version manager like `nvm` / `fnm`.

### Rust (latest stable)
```bash
# Install via rustup
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh

# Verify
rustc --version
cargo --version
```

### Tauri v2 system dependencies

**Windows**: Install the [WebView2 Runtime](https://developer.microsoft.com/en-us/microsoft-edge/webview2/) (pre-installed on Windows 11). Visual Studio Build Tools with the "Desktop development with C++" workload are required for Rust compilation.

**macOS**: Xcode Command Line Tools are required:
```bash
xcode-select --install
```

**Linux (Debian/Ubuntu)**:
```bash
sudo apt update
sudo apt install libwebkit2gtk-4.1-dev libappindicator3-dev librsvg2-dev patchelf
```

---

## Installation

### 1. Clone the repository
```bash
git clone <repo-url>
cd baki
```

### 2. Install root npm dependencies
```bash
npm install
```
This installs the Tauri CLI and root-level tooling.

### 3. Install frontend npm dependencies
```bash
cd frontend && npm install && cd ..
```

### 4. Verify Go dependencies
```bash
go mod download
```

After these steps the project is ready for development or production builds.

---

## Running in Development

```bash
npm run dev
```

This single command does three things in sequence:

1. **Compiles the Go sidecar** — runs `go run scripts/build-sidecar/main.go` which detects your current platform target triple (e.g. `x86_64-pc-windows-msvc`) and builds the Go binary to `src-tauri/bin/pad-backend-<triple>`.
2. **Starts the Vite dev server** — React frontend served at `http://localhost:5173` with HMR.
3. **Launches the Tauri window** — opens the desktop window pointing at the Vite dev server, with hot-reload for frontend changes.

> **Note**: The first run takes longer because Cargo compiles the Tauri Rust code. Subsequent runs are faster due to incremental compilation.

### Rebuilding the Go sidecar only
If you only change Go backend code and want to skip a full Tauri rebuild:
```bash
npm run build:sidecar
```
Then restart `npm run dev`.

### Frontend-only development
If you only need to work on the frontend without the desktop shell:
```bash
cd frontend
npm run dev
```
The frontend dev server starts at `http://localhost:5173`. Note that Tauri-specific APIs (file dialogs, OS keyring) will not be available in the browser — use the desktop app for full functionality.

---

## Building for Production

```bash
npm run build
```

This command:

1. Runs `npm run build:sidecar` — compiles the Go binary for the target platform.
2. Runs `npm run build --prefix frontend` — Vite compiles and bundles the React app into `frontend/dist/`.
3. Runs `tauri build` — Cargo compiles the Rust host, bundles the Go sidecar and frontend dist, and produces platform installers.

### Output locations

| Platform | Output |
|---|---|
| Windows | `src-tauri/target/release/bundle/msi/*.msi` and `nsis/*.exe` |
| macOS | `src-tauri/target/release/bundle/macos/*.app` and `dmg/*.dmg` |
| Linux | `src-tauri/target/release/bundle/appimage/*.AppImage` and `deb/*.deb` |

The standalone executable is at `src-tauri/target/release/baki` (or `baki.exe` on Windows).

### Cross-compilation

Tauri builds for the **current host platform** by default. Cross-compilation (e.g. building a Windows binary from macOS) is not officially supported — use a CI matrix or platform-specific machines.

### Debug build
To build without optimizations (faster compile, larger binary, with debug symbols):
```bash
cd src-tauri && cargo build
```

---

## Running Tests

### Go backend tests
Covers the parser, analyzer rules, AI provider factory, storage layer, and service layer:
```bash
go test ./...
```

Run with verbose output:
```bash
go test -v ./...
```

Run a specific package:
```bash
go test ./internal/parser/...
go test ./internal/analyzer/...
go test ./internal/ai/...
```

Run with race detector:
```bash
go test -race ./...
```

### Frontend tests (Vitest)
```bash
cd frontend
npm run test
```

Run in watch mode:
```bash
npm run test -- --watch
```

Run with coverage:
```bash
npm run test -- --coverage
```

### TypeScript type checking (no emit)
```bash
cd frontend
npx tsc --noEmit
```

---

## Project Structure

```
baki/
├── main.go                    # Go backend entry point
├── go.mod / go.sum            # Go module definition and checksums
├── package.json               # Root scripts (build:sidecar, dev, build)
│
├── frontend/                  # React TypeScript frontend
│   ├── package.json
│   ├── vite.config.ts
│   ├── tailwind.config.js
│   ├── tsconfig.json
│   └── src/
│       ├── main.tsx           # React entry point
│       ├── App.tsx            # Root component and layout
│       ├── index.css          # Global styles (Tailwind)
│       ├── api/               # HTTP/SSE client modules
│       ├── components/        # React components (~99 components)
│       │   ├── layout/        # Main layout (Sidebar, MainPane, Inspector, TitleBar)
│       │   ├── flow/          # Flow viewer components
│       │   ├── chat/          # AI chat panel
│       │   ├── findings/      # Analysis findings display
│       │   ├── inspector/     # Right-side inspector tabs
│       │   ├── settings/      # Settings modal panels
│       │   ├── sidebar/       # Left sidebar subcomponents
│       │   └── shared/        # Reusable shared components
│       ├── hooks/             # Custom React hooks
│       ├── stores/            # Zustand state stores
│       └── types/             # TypeScript domain types
│
├── internal/                  # Go backend packages
│   ├── ai/                    # AI provider integrations
│   ├── analyzer/              # Static analysis rule engine
│   ├── api/                   # HTTP router and handlers
│   ├── export/                # PDF and Markdown export
│   ├── manager/               # App orchestration
│   ├── models/                # Shared data structures
│   ├── parser/                # PAD format parser and tokenizer
│   ├── search/                # Full-text search engine
│   ├── service/               # Business logic layer
│   └── storage/               # OS keyring and filesystem persistence
│
├── src-tauri/                 # Tauri Rust host
│   ├── Cargo.toml
│   ├── tauri.conf.json        # Window config, sidecar config, file associations
│   ├── src/
│   │   ├── main.rs
│   │   └── lib.rs
│   └── bin/                   # Compiled Go sidecar binaries (git-ignored)
│
└── scripts/
    └── build-sidecar/
        └── main.go            # Cross-platform Go sidecar build script
```

---

## AI Provider Configuration

API keys and provider settings are configured in the application's **Settings** panel (`Ctrl + ,` → Providers tab). Keys are stored in the OS keyring (Windows Credential Manager, macOS Keychain, Linux libsecret) and never written to disk in plaintext.

### Supported providers

| Provider | Notes |
|---|---|
| GitHub Copilot | OAuth device flow (no key needed) or PAT |
| GitHub Models | Requires a GitHub PAT with `models:read` scope |
| Anthropic Claude | API key from console.anthropic.com |
| OpenAI | API key from platform.openai.com |
| Google Gemini | API key from aistudio.google.com |
| xAI Grok | API key from console.x.ai |
| Zhipu AI (GLM) | API key from open.bigmodel.cn |

---

## Keyboard Shortcuts

| Shortcut | Action |
|---|---|
| `Ctrl + O` | Open file or folder |
| `Ctrl + ,` | Application settings |
| `Ctrl + K` | Global search and command palette |
| `Ctrl + Shift + A` | Execute analysis rules |
| `Ctrl + E` | Export to PDF |
| `Ctrl + Shift + E` | Export to Markdown |
| `?` | Display shortcut reference |

---

## Troubleshooting

### `npm run dev` fails with "sidecar not found"
The Go binary must be built before Tauri starts. Run:
```bash
npm run build:sidecar
```
Then retry `npm run dev`.

### Tauri window is blank / white screen
The Vite dev server may not have started before the Tauri window opened. Wait a few seconds and reload the window (`Ctrl + R` inside the app), or check that port `5173` is not in use.

### Go test failures on Windows (keyring tests)
Tests that touch the OS keyring require a logged-in desktop session. Running them in a headless CI environment (e.g. a plain `cmd.exe` without a Windows session) may fail. Use:
```bash
go test ./... -run "^(?!TestKeyring)"
```
to skip keyring tests in CI.

### Rust build errors: missing WebView2
On Windows, install the [WebView2 Evergreen Runtime](https://developer.microsoft.com/en-us/microsoft-edge/webview2/). It is included by default in Windows 11 and Windows 10 21H2+.

### `cargo build` is slow on first run
The initial Rust compilation downloads and compiles all Tauri crates. This is a one-time cost of several minutes. Subsequent incremental builds are fast.

### Frontend type errors after pulling changes
Dependencies may have changed. Run:
```bash
cd frontend && npm install
```
Then recheck with `npx tsc --noEmit`.
