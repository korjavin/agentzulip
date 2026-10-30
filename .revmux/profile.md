# Project profile

Public repository. `zm` is a Go CLI plus a long-running daemon that bridges coding agents and a Zulip
server: it polls Zulip, types wake lines into terminal sessions via `zmx send`, runs external hook
commands (restart, unstick) with timeouts, and DMs the owner. Python helper scripts under
`skill/*/scripts/` parse agent transcripts and send data to an external batch API with a key read from
a secret store at run time. Markdown under `skill/` is executed by AI agents as instructions.

Calibration:

- The daemon runs unattended for days. A goroutine leak, a blocked channel in the event loop, a hook
  without a timeout, or a wake line typed into the wrong session is **major**.
- Anything that lets text from Zulip, a terminal screen or an LLM reply become keystrokes or a shell
  command beyond a fixed whitelist is **critical**.
- Secrets come only from the secret store at the point of use and are never printed, logged or
  written to disk; a violation is **critical**.
- The repo is public: any real hostname, domain, email (other than the owner's public handle
  `korjavin`), secret-store key/path, team or agent name, or private project name in the diff is
  **major**.
- `SKILL.md` and other agent-facing Markdown are contracts: a command, flag, default or path that does
  not match the code is rated by what an agent following it does wrong.
- Stdlib-first, minimal code; do not ask for abstractions, extra tests or docs the change does not need.
