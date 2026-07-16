# Security Policy

## Reporting a Vulnerability

PAD Analyzer is a static analysis tool for Power Automate Desktop flows. If you
discover a security vulnerability in the analyzer itself (e.g., a crafted flow
file that crashes the parser, an authentication bypass, or an injection vector
in the web UI), please report it responsibly.

**Do NOT open a public GitHub issue for security vulnerabilities.**

Instead, please email security@sftfox.com with:

1. A description of the vulnerability and its potential impact
2. Steps to reproduce (a minimal flow file or HTTP request)
3. The version/commit you tested against
4. Any suggested mitigations

We will acknowledge receipt within 48 hours and provide an initial assessment
within 5 business days.

## Scope

**In scope:**
- Crashes or resource exhaustion (OOM, infinite loops) caused by parsing a
  crafted `.txt` flow file
- Authentication or authorization bypass in the web/desktop API
- Injection vulnerabilities (SQL injection, XSS, SSRF) in the web UI
- Secrets leakage in logs or API responses

**Out of scope:**
- Findings reported by PAD Analyzer about your own flows (those are the tool
  working as designed — use the suppression mechanism)
- Vulnerabilities in third-party dependencies (report upstream)
- Social engineering or physical attacks

## Disclosure

We follow coordinated disclosure. Once a fix is available, we will:
1. Publish a patched release
2. Credit the reporter (unless they prefer to remain anonymous)
3. Disclose the vulnerability details after users have had time to update
