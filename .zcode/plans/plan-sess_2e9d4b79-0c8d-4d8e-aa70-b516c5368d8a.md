Remove dependabot[bot] as a contributor by reassigning its 14 commits (all on remote dependabot/* branches; none on master) to Dusan Kuburic. Master's 85 commits are already authored by you and will keep their exact hashes — no force-push of master, no clone invalidation for anyone working off master.

**Step 0 — Precondition: deal with the 60 staged changes.** The working tree has staged work from earlier sessions; a filter-repo run requires a clean tree. Commit it as one WIP commit on master (content untouched).

**Step 1 — Safety backup.** `git bundle create ../baki-pre-rewrite.bundle --all` — a single-file copy of the entire pre-rewrite history. Everything is recoverable from it (and from origin) until GitHub garbage-collects.

**Step 2 — Install git-filter-repo** (`pip install git-filter-repo`) — the officially recommended rewrite tool (fast, refuses unsafe runs). Not currently installed.

**Step 3 — Authorship rewrite via mailmap.** Create a mailmap mapping `dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>` → `Dusan Kuburic <dusan.kuburic@outlook.com>`, then run `git filter-repo --mailmap <file>` across all refs. Commits whose identity doesn't match the mailmap (all of master) come out byte-identical with unchanged hashes; only the 14 dependabot commits get new authors and new hashes. Committer identity of those commits (currently `GitHub <noreply@github.com>`) is also normalized to you so no GitHub-bot identity remains.

**Step 4 — Push.** filter-repo removes the origin remote as a safety measure; re-add it, then force-push only the rewritten `dependabot/*` branches. Master is NOT force-pushed (hashes unchanged). The 14 open Dependabot PRs on GitHub will re-attach to the rewritten branches and display you as author.

**Step 5 — Verify.** `git shortlog -sne --all` shows a single contributor locally; after GitHub refreshes (can take up to ~24h for the contributors cache), the repo's contributor list shows only you.

**Important caveat — Dependabot will come back.** As long as `.github/dependabot.yml` is active, future Dependabot runs create NEW branches/PRs authored by dependabot[bot] and it reappears as a contributor. Options (your choice, can be done after): merge or close the 14 rewritten PRs, and/or disable Dependabot by removing/commenting `dependabot.yml`. I'll flag this at execution and you tell me which way to go.

Risks covered: full bundle backup before any rewrite; no master force-push; no tags exist (nothing else to rewrite); local repo is the only clone we know of (the user's GitHub remote keeps pre-rewrite commits until GC, so a window exists to undo).