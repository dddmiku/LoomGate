# LoomGate engineering rules

- Communicate in simplified Chinese. Credentials, cookies, tokens, private keys, verification codes and phone numbers must never appear in output, source, logs or commits.
- The legacy source under Desktop/na/workbuddy2api-git-update is read-only. Preserve compatibility with v3.16.1 baseline commit 4b6452e5e0e4c49b45604fd878f6174701bce5eb.
- Source belongs here. Private captures, local runtime state and builds belong in D:/LoomGate-work, never TEMP or this repository.
- Claude owns implementation and verification following the latest user handoff; do not dispatch further work to Codex. Formal release and production cutover still require full acceptance. Do not tag, release, change production, rotate signing identities or archive the legacy repository.
- Work on codex/rebuild. Never claim a test, real account flow or platform build passed unless it ran and evidence exists.
- Go 1.26.8, GOMAXPROCS=2, -mod=readonly -p=1. Coordinate heavy builds; use D:/LoomGate-work/tmp.
- Task definition/discovery/execution/claim/state/scheduling must be separated. Catalogs and task IDs come from providers; no hard-coded fake upstream models or optimistic completion.
- Keep local CHECKPOINT.md in D:/LoomGate-work current. Changes in this repository must not include private capture material or inherited release automation.
