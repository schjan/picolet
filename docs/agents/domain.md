# Domain docs

This repository uses a single context: the root `CONTEXT.md` is the domain glossary, and `docs/adr/` holds architecture decisions.

## Before exploration

Read `CONTEXT.md` and the ADRs relevant to the area being explored. If a document is absent, proceed; domain-modeling creates documentation when terms or decisions are resolved.

## Vocabulary

Use the glossary's terms in issue titles, briefs, hypotheses, tests and proposals. Respect its distinctions, particularly Machine, Host, Agent, Service Bundle, Managed File and Timer-triggered One-shot. If a needed concept is missing, reconsider whether it is a new concept or an existing one under another name; record a real gap for domain-modeling.

## Decisions

Surface conflicts with accepted ADRs explicitly before proposing a different direction. Name the affected ADR and the reason to revisit it. Read historical design documents in light of the accepted ADRs and any supersession notices.
