# Service — API layer (`internal/api`)

Thin HTTP boundary. Parses, authenticates (mTLS/API key), calls one engine function, serialises a
Pydantic-equivalent JSON response. No SMTP, Redis, or classification logic here (ENGINEERING-STANDARDS §2).

- Endpoints & shapes: `docs/06-generated/api.md` (the contract).
- Auth: every route except `/healthz` `/readyz`. Stub in plan 000, real in plan 001.
- Error shape: `{error:{code,message}}`; a bad request is `400`, never a verification `unknown`.
- The engine it calls is transport-agnostic, so the same code serves the queue worker later (007).
