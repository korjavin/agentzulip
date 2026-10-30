---
description: security — untrusted text reaching a terminal, a shell or a model; secrets handling; subprocess and timeout hygiene
---
## Lens: security

Trace every value the change takes from outside the process to where it is used. Outside means:
Zulip message text, terminal screen captures, LLM output, transcript files, environment variables,
config files, command-line arguments, and output of any subprocess.

Look for:

- **keystroke / terminal injection** — text that ends up typed into a terminal session (`zmx send`,
  `tmux send-keys` or similar) or sent as keys. Screen contents and LLM replies are attacker-shaped:
  check that only an explicit whitelist of keys or fixed strings can be sent, that control characters,
  escape sequences and newlines are stripped or rejected, and that a model's free text can never be
  passed through verbatim
- **command injection** — untrusted data interpolated into `sh -c`, `bash -c`, `eval`, backticks,
  `subprocess(..., shell=True)`, `exec.Command("sh", "-c", ...)`, systemd `ExecStart=` lines, or
  rendered templates; option injection (a value starting with `-` passed as an argument without `--`)
- **secrets** — API keys and tokens must be read only from the secret store (`kv` / stash) at the
  point of use. Flag any key that is printed, logged, echoed, put in an error message, written to a
  file, passed on a command line visible in `ps`, exported to child processes that do not need it,
  committed, or given a hard-coded fallback value
- **data sent off-host** — what exactly is uploaded to an external API (e.g. a batch API); flag
  secrets, tokens or credentials that can ride along inside the uploaded payload
- **subprocess hygiene** — hooks or external commands run without a timeout, a timeout that kills the
  parent but leaves the child process group running, unbounded reads of child output
- temp files created with predictable names or world-readable permissions when they hold anything
  sensitive

For each finding name the source of the untrusted value, the sink, and the concrete string that does
damage. A sink fed only by values the owner writes in their own config is not a vulnerability.
