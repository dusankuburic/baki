# 1. Record architecture decisions

Date: 2026-10-06. Status: accepted.

## Context

The backend's design decisions live in code comments and commit messages, which explain *what* but rarely the
alternatives that were rejected or the consequences that were accepted. The refactoring planned in
`docs/architecture/backend-refactoring-proposal.md` makes several decisions that later work must respect.

## Decision

Record each decision with lasting consequences as a short ADR in `docs/adr/`, numbered in order: context,
decision, consequences. An ADR is never edited after acceptance except to mark it superseded by a later one.

## Consequences

Reviewers can point at the ADR instead of reconstructing the reasoning. ADRs cost a few minutes each, so they
are kept for decisions that constrain future work, not for routine changes.
