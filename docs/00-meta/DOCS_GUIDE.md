# Docs guide — what each folder is for

Mirrors the Data Scout layout so both repos navigate the same way.

| Folder | Holds | Updated |
|---|---|---|
| `00-meta/` | Orientation: `AGENTS.md`, this guide, `CONTRIBUTING.md` | rarely |
| `01-product/` | What the service does for users of the platform: `index.md`, `features/NNN-*.md`, `references/` (ported analyses) | per feature |
| `02-architecture/` | How it's built: `ARCHITECTURE.md`, `ENGINEERING-STANDARDS.md`, `service/*.md` (per component), `decisions/ADR-*.md` | per architectural change |
| `03-engineering/` | How to build it: `patterns/*.md`, `operations/*.md`, `testing/*.md` | per pattern/op |
| `04-execution/` | The work: `ROADMAP.md`, `tech-debt.md`, `exec-plans/{planned,active,completed,templates}/` | continuously |
| `05-quality/` | Gates: `checklists/pr-checklist.md`, `SECURITY.md` | per quality change |
| `06-generated/` | Contracts derived from code: `api.md`, `redis-contract.md`, `metrics.md`. **Kept in sync with code, in the same change.** | per code change |
| `07-references/` | Operational + external: `RUNBOOK.md`, the limiter contract, ported reference docs | as needed |
| `08-decisions/` | `changelog.md` — one entry per plan, always | per plan |

## Rules

- **`06-generated/` is a contract, not a diary.** When an endpoint, Redis key, or metric changes,
  update the matching file in the same change. A stale contract is a bug.
- **Every plan writes a `changelog.md` entry**, even when nothing surprising happened.
- **ADRs are immutable once Accepted.** A reversal is a new ADR that supersedes, linking back.
- Exec-plans move `planned/ → active/ → completed/`; a plan is "done" only when its Status says so
  and it lives in `completed/`.
