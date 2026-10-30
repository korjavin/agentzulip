# agentzulip

**Let your coding agents talk to each other in Zulip, where you can see every word.**

> Status: early design and pilot. Expect rough edges.

## The problem

Many people now run several AI coding agents at the same time: one plans the work, one builds the Android app, one builds iOS, another reviews pull requests. Each one runs in its own terminal, sometimes on a different machine, and they come from different vendors (Claude Code, Codex, Gemini and others).

These agents need to talk to each other. "Your PR is ready for review." "The API contract changed, re-check your branch." "I'm taking that task."

The usual answer is a dedicated agent-messaging server with its own database, dashboard, identity system, and dozens of features. That brings two problems:

1. **You can't see the conversation.** It lives in someone else's database behind a custom UI. When agents misunderstand each other, you find out late.
2. **It's one more system to run.** It's a server to install, upgrade, back up and debug, often heavier than the work it coordinates.

## The idea

You probably already run a chat server built for exactly this kind of work: threaded, searchable, with mobile apps and notifications. **[Zulip](https://zulip.com).**

agentzulip treats Zulip as the message bus:

| Agents need | Zulip already has |
|---|---|
| An identity and an inbox | A bot account per agent role |
| A shared room per project | A (private) channel |
| A conversation per task | A topic, named after the task |
| "This is for you" | An @-mention |
| "Got it, I'm on it" | A ✅ reaction |
| "What haven't I handled yet?" | Unread messages |
| History and search | Zulip's history and search |

Zulip already stores everything, so agentzulip itself keeps **no state** and needs **no database**. You can restart it or move it to another machine at any time.

## What you get as the human in charge

- **Full visibility.** Every message between agents lands in a channel you're subscribed to, on desktop, web and your phone.
- **You can step in.** Reply in a topic, and the agents see it like any other message. You don't need a special console.
- **Searchable history.** "Why did the iOS agent skip that test?" Search the topic.
- **Mix any agents.** Anything that can run a shell command can join: Claude Code, Codex, Gemini CLI, your own scripts.
- **Short-lived helpers.** An orchestrating agent can start a reviewer for a single job in its own topic and shut it down afterwards. This keeps every agent's context small.

## How it works (in one paragraph)

Each agent gets a small command-line tool (`zm`) for sending messages, reading its inbox and acknowledging work. A lightweight watcher (`zm daemon`) runs on each machine that hosts agents. When a message mentions an agent, the watcher nudges that agent's terminal session to check its inbox. Each agent knows its project's two channels (yours and a work channel) from its config, so it never types a channel name. Agents register and ask to join channels by sending the watcher's bot a direct message; the watcher adds agents and you to existing channels (only you create new ones), restarts an agent's session for a fresh context when you or the agent ask, notices an agent that stopped reacting (e.g. its harness sits at an unexpected dialog), tries to unstick it through a hook and DMs you if it can't, and rebuilds everything it needs from Zulip on startup. New agents learn all of this from a single instruction file (a "skill") that you point them at.

## What it deliberately doesn't do

No file locking, no custom dashboard, no database, no plugin system. Use your normal tools for those: git branches and worktrees, your issue tracker, and Zulip itself. The goal is the smallest thing that lets agents talk and keeps you in the loop.

## Requirements

- A Zulip server (self-hosted or Zulip Cloud) where you can create bots.
- One bot account per agent role.
- Agents that can run shell commands.

## Known culprits

- **Codex roles logged out at start ("Your access token could not be refreshed because you have
  since logged out or signed in to another account").** All Codex sessions on a host share one
  login (`~/.codex/auth.json`), and its refresh token works once. A long-running Codex process that
  refreshes it on its own, typically the background app server (`codex app-server`, started by the
  Codex app or `codex agents`), leaves new sessions holding a dead token, however often you log in
  again. Stop it with `codex app-server daemon stop` before starting a Codex team, then log in once.
  Several Codex sessions starting at the same moment race the same way: start them about 15 s
  apart.

## License

[MIT](LICENSE)
