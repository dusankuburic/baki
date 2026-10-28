package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/storage/database"
)

// ---- bakicli keys ----

// runKeys dispatches the keys subcommands (check).
func runKeys(args []string) {
	if len(args) == 0 || args[0] != "check" {
		printKeysUsage()
		os.Exit(2)
	}
	runKeysCheck(args[1:])
}

func printKeysUsage() {
	fmt.Fprint(os.Stderr, `usage: bakicli keys check [--dsn <postgres-url>]

Reports which configured key opens each stored sealed value (provider keys, organisation
credentials, the sealed secret columns), so the stored values can be re-sealed before a
PAD_AUTH_SECRET rotation or before PAD_ENCRYPTION_KEY_PREVIOUS is removed. It reads the
keys from the server's environment (PAD_ENCRYPTION_KEY, PAD_ENCRYPTION_KEY_PREVIOUS,
PAD_AUTH_SECRET), names them, and never prints one. Read-only.

Exit codes: 0 every sealed value opens with a key the server uses, and rotating
PAD_AUTH_SECRET leaves them readable; 1 action needed (printed); 2 usage error.
`)
}

// sealKeyCandidate is a key the check tries and what it is to the server.
type sealKeyCandidate struct {
	database.SealKey
	role sealKeyRole
}

type sealKeyRole int

const (
	roleCurrent  sealKeyRole = iota // PAD_ENCRYPTION_KEY: seals and opens
	rolePrevious                    // PAD_ENCRYPTION_KEY_PREVIOUS: opens until the re-seal moves its values
	roleFallback                    // PAD_AUTH_SECRET while PAD_ENCRYPTION_KEY is unset: the server's keystore key
	roleAuthOnly                    // PAD_AUTH_SECRET beside PAD_ENCRYPTION_KEY: the server does not try it
)

// sealKeyCandidates lists the configured keys in the order the server tries them, then the auth secret when the
// server does not use it (to find values a move off the fallback left behind). A key whose value is also
// PAD_AUTH_SECRET's says so in its name and is listed once.
func sealKeyCandidates(a config.AuthConfig) []sealKeyCandidate {
	auth := a.Secret.Reveal()
	name := func(base, value string) string {
		if auth != "" && value == auth {
			return base + " (same value as PAD_AUTH_SECRET)"
		}
		return base
	}
	var out []sealKeyCandidate
	add := func(n, v string, role sealKeyRole) {
		if v != "" {
			out = append(out, sealKeyCandidate{database.SealKey{Name: n, Key: []byte(v)}, role})
		}
	}
	if !a.EncryptionKey.IsSet() {
		add("PAD_AUTH_SECRET (the keystore key while PAD_ENCRYPTION_KEY is unset)", auth, roleFallback)
		for i, p := range a.EncryptionKeyPrevious {
			add(fmt.Sprintf("PAD_ENCRYPTION_KEY_PREVIOUS entry %d", i+1), p.Reveal(), rolePrevious)
		}
		return out
	}
	usesAuth := a.EncryptionKey.Reveal() == auth
	add(name("PAD_ENCRYPTION_KEY", a.EncryptionKey.Reveal()), a.EncryptionKey.Reveal(), roleCurrent)
	for i, p := range a.EncryptionKeyPrevious {
		add(name(fmt.Sprintf("PAD_ENCRYPTION_KEY_PREVIOUS entry %d", i+1), p.Reveal()), p.Reveal(), rolePrevious)
		usesAuth = usesAuth || p.Reveal() == auth
	}
	if !usesAuth {
		add("PAD_AUTH_SECRET", auth, roleAuthOnly)
	}
	return out
}

// runKeysCheck implements `bakicli keys check`.
func runKeysCheck(args []string) {
	fs := flag.NewFlagSet("keys check", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN (default: $PAD_DATABASE_URL)")
	fs.Usage = printKeysUsage
	_ = fs.Parse(args)

	cfg, err := config.LoadFromEnvRaw()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli keys check: %v\n", err)
		os.Exit(2)
	}
	dsn := *dsnFlag
	if dsn == "" {
		dsn = cfg.Storage.DatabaseURL.Reveal()
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "bakicli keys check: no database DSN (set --dsn or PAD_DATABASE_URL)")
		os.Exit(2)
	}
	candidates := sealKeyCandidates(cfg.Auth)
	if len(candidates) == 0 {
		fmt.Fprintln(os.Stderr, "bakicli keys check: no key configured (set PAD_ENCRYPTION_KEY, PAD_ENCRYPTION_KEY_PREVIOUS and PAD_AUTH_SECRET as the server has them)")
		os.Exit(2)
	}

	ctx := context.Background()
	b, err := database.OpenForMaintenance(ctx, database.DefaultConfig(dsn))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli keys check: connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = b.Close() }()

	keys := make([]database.SealKey, len(candidates))
	for i, c := range candidates {
		keys[i] = c.SealKey
	}
	rep, err := database.CheckSealKeys(ctx, b.DB(), keys)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli keys check: %v\n", err)
		os.Exit(1)
	}
	if !printKeysReport(os.Stdout, candidates, rep) {
		os.Exit(1)
	}
}

// printKeysReport prints the check and what to do; false when action is needed before a PAD_AUTH_SECRET rotation (or
// now: values the server cannot open).
func printKeysReport(w *os.File, candidates []sealKeyCandidate, rep database.SealKeyReport) bool {
	fmt.Fprintln(w, "Keys tried, in order (named, never printed):")
	for i, c := range candidates {
		fmt.Fprintf(w, "  %d. %s\n", i+1, c.Name)
	}
	fmt.Fprintln(w, "\nSealed values by column:")
	for _, col := range rep.Columns {
		if col.Checked == 0 && col.Plaintext == 0 {
			continue
		}
		parts := []string{fmt.Sprintf("%d checked", col.Checked)}
		for _, c := range candidates {
			if n := col.OpenedBy[c.Name]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", c.Name, n))
			}
		}
		if col.Unreadable > 0 {
			parts = append(parts, fmt.Sprintf("no key %d", col.Unreadable))
		}
		if col.Plaintext > 0 {
			parts = append(parts, fmt.Sprintf("plaintext %d", col.Plaintext))
		}
		fmt.Fprintf(w, "  %s.%s: %s\n", col.Table, col.Column, strings.Join(parts, "; "))
	}
	fmt.Fprintf(w, "\nTotal: %d checked, %d unreadable, %d plaintext. Opened by:\n", rep.Checked, rep.Unreadable, rep.Plaintext)
	for _, c := range candidates {
		fmt.Fprintf(w, "  %s: %d\n", c.Name, rep.OpenedBy[c.Name])
	}
	fmt.Fprintln(w)

	ok := true
	pending := 0
	for _, c := range candidates {
		n := rep.OpenedBy[c.Name]
		switch {
		case n == 0:
		case c.role == roleFallback:
			ok = false
			fmt.Fprintf(w, "ACTION NEEDED: %d stored value(s) are sealed under PAD_AUTH_SECRET, the keystore key while PAD_ENCRYPTION_KEY is unset: "+
				"rotating PAD_AUTH_SECRET now makes them unreadable. Set PAD_ENCRYPTION_KEY=<a new random value: openssl rand -hex 32> and "+
				"PAD_ENCRYPTION_KEY_PREVIOUS=<the current PAD_AUTH_SECRET value>, deploy, wait for the log line \"secret re-seal complete\", "+
				"then run this check again (docs/architecture/operations.md, Encryption keys).\n", n)
		case c.role == roleAuthOnly:
			ok = false
			fmt.Fprintf(w, "ACTION NEEDED: %d stored value(s) open only with PAD_AUTH_SECRET, which the server does not try while PAD_ENCRYPTION_KEY "+
				"is set, so they are unreadable to it now. Add the PAD_AUTH_SECRET value to PAD_ENCRYPTION_KEY_PREVIOUS, so the next boot "+
				"re-seals them, and do not rotate PAD_AUTH_SECRET before this check shows none.\n", n)
		case c.role == rolePrevious:
			pending += n
		}
	}
	if rep.Unreadable > 0 {
		ok = false
		refs := make([]string, len(rep.UnreadableRefs))
		for i, r := range rep.UnreadableRefs {
			refs[i] = fmt.Sprintf("%s.%s[%s]", r.Table, r.Column, r.RowID)
		}
		fmt.Fprintf(w, "ACTION NEEDED: %d stored value(s) open with none of these keys (sealed under a key no longer configured): %s. "+
			"Add that key to PAD_ENCRYPTION_KEY_PREVIOUS if it is known; otherwise the owners re-enter those credentials.\n", rep.Unreadable, strings.Join(refs, ", "))
	}
	if pending > 0 {
		fmt.Fprintf(w, "re-seal pending: %d value(s) still open only with a PAD_ENCRYPTION_KEY_PREVIOUS entry; each boot's re-seal pass moves them to "+
			"PAD_ENCRYPTION_KEY. Keep PAD_ENCRYPTION_KEY_PREVIOUS until this check shows none.\n", pending)
	}
	if rep.Plaintext > 0 {
		fmt.Fprintf(w, "note: %d value(s) are stored unsealed (written before field encryption); the boot's at-rest backfill seals them.\n", rep.Plaintext)
	}
	for _, c := range candidates {
		if c.role == roleFallback {
			fmt.Fprintf(w, "note: PAD_ENCRYPTION_KEY is unset; in cloud mode that fallback is deprecated and refused from the first release on or after %s.\n", config.EncryptionKeyFallbackRemoval)
		}
		if c.role == roleCurrent && strings.HasSuffix(c.Name, "(same value as PAD_AUTH_SECRET)") {
			fmt.Fprintln(w, "note: PAD_ENCRYPTION_KEY has PAD_AUTH_SECRET's value: rotating PAD_AUTH_SECRET is safe (the keystore keeps its own variable), but a leak of one is a leak of both; rotate PAD_ENCRYPTION_KEY with PAD_ENCRYPTION_KEY_PREVIOUS.")
		}
	}
	if ok {
		fmt.Fprintln(w, "ok: every sealed value opens with a key the server uses, and rotating PAD_AUTH_SECRET leaves them readable.")
	}
	return ok
}
