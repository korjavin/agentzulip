---
description: agent-facing docs as contracts — SKILL.md and reference text must match what the CLI, scripts and templates actually do
---
## Lens: agentdocs

Skill files (`SKILL.md`, `references/*.md`, role cards and other Markdown that instructs an AI agent)
are executed by agents as instructions. Treat them as a contract: a wrong instruction makes an agent
run a wrong command. Rate a mismatch by what the agent following it would do wrong, not as prose.

For every command, flag, subcommand, file path, config key, environment variable, default value,
exit code, timeout or output format the changed docs mention, find it in the code and confirm it:

- the flag or subcommand exists, takes that argument shape, and behaves as described
- defaults, timeouts and limits stated in the docs equal the ones in the code
- file paths and names the docs tell the agent to create, read or edit are the ones the code uses
- the order of steps works — nothing is used before the step that creates it
- behavior the change added or altered in code is reflected in the skill text an agent reads, and
  behavior the docs still describe has not been removed from the code
- examples would run as written

Also check the reverse direction on the changed code: a new or changed user-visible behavior that the
agent-facing docs should mention but do not. Ignore wording and style.
