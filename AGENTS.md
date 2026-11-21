# LoomGate engineering rules

- Communicate in simplified Chinese. Credentials, cookies, tokens, private keys, verification codes and phone numbers must never appear in output, source, logs or commits.
- The legacy source under Desktop/na/workbuddy2api-git-update is read-only. Preserve compatibility with v3.16.1 baseline commit 4b6452e5e0e4c49b45604fd878f6174701bce5eb.
- Source belongs here. Private captures, local runtime state and builds belong in D:/LoomGate-work, never TEMP or this repository.
- Follow the latest user handoff for implementation and verification. Formal releases require complete release acceptance. Production cutover requires its own authorization; preserve signing identities and the legacy repository.
- Work on codex/rebuild. Never claim a test, real account flow or platform build passed unless it ran and evidence exists.
- The user authorized automatic source pushes and formal releases on 2026-11-21: after a requested fix passes verification, commit and push its task changes to origin/codex/rebuild, complete exact-source release acceptance, build and sign the update assets with the existing identity, and publish a new versioned GitHub Release without asking again. Verify the remote tag, latest release, anonymous asset downloads, checksums and signature so the workbench updater can discover the release. This authorization continues for subsequent requested fixes in this project.
- Use the parameterized local release pipeline in D:/LoomGate-work/tools (prepare_release_candidate.py and ship_oneclick_release.py). Complete acceptance before signing or pushing a new release tag. The current GitHub build workflow is manual unsigned acceptance only; a source push alone does not publish a hot update. Never force push, replace an existing tag or release asset, change repository visibility, rotate signing identities, publish private files, or deploy/restart production as part of release publication.
- Go 1.26.8, GOMAXPROCS=2, -mod=readonly -p=1. Coordinate heavy builds; use D:/LoomGate-work/tmp.
- Task definition/discovery/execution/claim/state/scheduling must be separated. Catalogs and task IDs come from providers; no hard-coded fake upstream models or optimistic completion.
- Keep local CHECKPOINT.md in D:/LoomGate-work current. Changes in this repository must not include private capture material or inherited release automation.
