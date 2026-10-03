# Slack setup

## Create and install the app

1. Enable Socket Mode. Grant the app token `connections:write`.
2. Grant bot scopes `chat:write`, `reactions:read`, `reactions:write`, and
   `channels:history`; use `groups:history` for a private posting channel.
3. Subscribe to `reaction_added`, install the app, and invite the bot to the
   destination channel.
4. Provision an app token (`xapp-…`) and bot token (`xoxb-…`) in the daemon environment.

```kdl
slack "C0123456789" {
    app-token-env "SLACK_APP_TOKEN"
    bot-token-env "SLACK_BOT_TOKEN"
    allowed-users "U025FTHN3" "U0987ZYXWV"
}
```

The token variable names shown are defaults. A configured channel requires both
tokens at startup. Configured secrets are removed from candidate environments.
Without `allowed-users`, anyone who can react in the channel can issue reaction
commands; with it, only those IDs are admitted by the configured allowlist under
shipped policy. Rego owns the [command decision](../reference/policy.md).

## Verify a run

Push a disposable candidate. The bot posts a root message, check replies, and a
terminal outcome. A retry reuses the root thread.

| Reaction on a run root | Action |
|---|---|
| `:recycle:` | Retry the ref's current revision. |
| `:x:` | Cancel and park the ref. |

These work after a run has finished because root ownership is stored in message
metadata. History permission is needed to retrieve that metadata; without it,
reactions on finished runs cannot be resolved. `users:read` is unnecessary.

A multi-member batch root cannot identify one member. The bot replies with
API/CLI guidance instead of applying the reaction to every member. Select the
member in the dashboard or use [retry/cancel](landing.md#retry-or-cancel).
