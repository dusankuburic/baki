# Release artifacts — verifying integrity

Every `bakicli` release archive published to GitHub Releases is accompanied by a
SHA256 sidecar `<asset>.sha256` so consumers can verify download integrity
without trusting GitHub's transport.

## Verify a downloaded binary

```bash
# Download the archive and its sidecar from the GitHub Release page, then:
sha256sum -c bakicli-<goos>-<goarch>.tar.gz.sha256
# → bakicli-<goos>-<goarch>.tar.gz: OK
```

The sidecar is produced by `sha256sum` so the format is compatible with the
coreutils tool on Linux, macOS, and Windows (Git Bash / WSL). On Windows
PowerShell use `Get-FileHash` and compare manually:

```powershell
$expected = (Get-Content bakicli-windows-amd64.zip.sha256).Split(' ')[0]
$actual   = (Get-FileHash bakicli-windows-amd64.zip -Algorithm SHA256).Hash.ToLower()
if ($expected -eq $actual) { "OK" } else { "MISMATCH" }
```

## How the sidecar is produced

In `.github/workflows/release.yml`, immediately after archiving the binary for
each OS/arch matrix job:

```bash
sha256sum "$ext" > "$ext.sha256"
```

Both `<asset>` and `<asset>.sha256` are uploaded to the GitHub Release via
`softprops/action-gh-release`.

## Signing

Releases are NOT currently GPG / cosign signed. The SHA256 sidecar defends
against transport-level corruption and accidental publisher-side tampering; it
does NOT defend against a compromised release workflow (an attacker who can
push to the release job can recompute both the archive and the sidecar).

Future hardening (deferred): introduce keyless cosign signing via the
`sigstore/cosign-installer` action with OIDC identity provenance so consumers
can verify the archive's signature against the workflow's GitHub Actions
identity, not just a publisher-supplied hash.

## Updating pinned GitHub Actions

Every `uses:` declaration across `.github/workflows/*.yml` and `action.yml` is
pinned to a 40-character commit SHA with the original tag preserved as a
trailing comment:

```yaml
- uses: actions/checkout@34e114876b0b11c390a56381ad16ebd13914f8d5  # v4
```

This defends against tag-rewrite supply-chain attacks (cf. the 2025
`tj-actions/changed-files` compromise). Dependabot's `github-actions` ecosystem
watches these refs and opens PRs that update both the SHA and the comment
together when a new tag is published — see `.github/dependabot.yml`.
