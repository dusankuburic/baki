Verify the deployed resize fix with a recorded video, and fix anything that still moves

What I found (read-only)
=======================
- Your tab IS on the latest build (index-BDAPH5NV.js) — reload worked.
- But your session is at the LOGIN screen: you were signed in as
  resize-test@example.com — the throwaway account I created for diagnosis and then
  deleted in cleanup, which logged you out. Sorry about that — my cleanup crossed
  into an account you were actively using.
- Consequence: the toolbar freeze fix (deployed ~20 min ago) has probably never been
  exercised by you with a flow loaded. Your "still same issue" test ran on the
  previous build (canvas fix only).
- Static re-check of remaining overlay candidates: the main toolbar strip is a fixed
  single row (no wrap) — not a mover. The floating map toolbar was the wrap offender
  and is pinned in the deployed build; the legend is width-constant.

Plan (needs write access)
=========================
1. Recreate the account you were using — same email (resize-test@example.com) and
   same password (ResizeTest-2026x!) — via /api/auth/register + a DB-minted email
   verification token (same recipe as before), so you can sign straight back in.
2. Sign in in a controlled browser tab, load a flow with subflows, switch to Map
   and Local Map.
3. Record a WebM video of the app while performing the exact repro: real sidebar
   divider drag, inspector divider drag, and window resize steps. Watch the
   recording frame by frame to see every visible blink/shift with my own eyes
   instead of inferring from code.
4. If anything visibly moves/blinks in the recording: fix precisely that (likely
   candidates: the focused-node side card sliding with the pane edge, the search
   pill, anything else recorded), re-record to confirm clean.
5. Gates: vitest on touched files, tsc, eslint, the 4-test graph-resize e2e suite;
   rebuild the container if anything changed; verify served bundle hash.
6. If the recording is already clean: the deployed build resolves the drag issue —
   hand the recreated login back to you to confirm on your side.
7. Cleanup rule learned: never delete the account again without asking; test data
   (flows) only.

Note: if what you resize is the ZCode in-app browser pane itself rather than the
app's dividers, tell me — that path resizes the app viewport (window resize), which
the deployed build also shields, and the recording in step 3 covers it either way.