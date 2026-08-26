# Changelog

One entry per plan (always), newest first: decisions made, deviations, library/provider choices,
trade-offs.

## 2026-08-26 — Repo bootstrap & architecture

- Created `email-verifier` as a standalone Go service repo with the Data Scout docs layout.
- **Architecture locked** via ADRs 001–004:
  - ADR-001 — build on the `ds-smtp-retry` Go engine (mature AIMD/bands/classifier/central bucket).
  - ADR-002 — scope = verification probe + outbound relay, phased (verify first, relay in Phase C);
    layers 0–5 stay in Data Scout.
  - ADR-003 — HTTP integration now (queue later); stateless about business data (Data Scout stores
    verdicts).
  - ADR-004 — one probe node at the start; the token bucket is central from plan 003 so scaling is
    deployment-only.
- Seeded docs: `CLAUDE.md`, `README.md`, `ARCHITECTURE.md`, `ENGINEERING-STANDARDS.md`, `AGENTS.md`,
  `DOCS_GUIDE.md`, product index, generated contracts (`api`/`redis-contract`/`metrics`), patterns
  (`smtp-classification`/`aimd-pacing`/`ssrf-guard`/`retry-greylist`), pr-checklist, ROADMAP, plan
  template, and plan 000.
- Ported references from `ds-smtp-retry`: `RUNBOOK.md`, `token_bucket.lua`; and the Data Scout
  build-vs-buy analysis (the decision record mandating a separate isolated-IP probe).
- No code yet — execution starts at plan 000 (scaffold-and-standards).
