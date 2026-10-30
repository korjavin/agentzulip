---
description: privacy for a public repository — no hostnames, domains, emails, team or agent names, secret-store paths or private project names
---
## Lens: privacy

This repository is **public**. Read every added or changed line of the diff — code, comments, tests,
fixtures, docs, skill text, commit-visible examples — and flag anything that identifies the owner's
private environment. Only the generic project itself and the owner's public handle `korjavin` may
appear.

Flag:

- hostnames, machine names, internal or private domains, IP addresses, URLs other than well-known public
  services and the project's own public repository
- email addresses other than korjavin's
- names of the owner's teams, agents, Zulip organizations, streams/channels or topics that are real
  rather than illustrative placeholders (`<agent>`, `example`, `alice`)
- secret-store key names or paths (stash / `kv` keys), home-directory paths with a real username,
  absolute paths to the owner's checkouts
- names of private projects, products, customers or repositories
- tokens, keys, session ids or anything that looks like a credential, even if expired
- real transcript, log or message excerpts pasted as examples

Severity: a credential is critical; a real hostname, email, secret-store path or private project name is
major (it is published the moment this is pushed); a borderline name is minor. Quote the exact string
and its file and line, and suggest a placeholder.
