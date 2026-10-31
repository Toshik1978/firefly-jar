# Data Model: Telegram Forum Topic and Silent Delivery

## config.Telegram (`notify.telegram:`)

| Field | YAML | Type | Default | Rule |
|---|---|---|---|---|
| BotTokenFile | `bot_token_file` | string | (unchanged) | (unchanged) |
| ChatIDs | `chat_ids` | []int64 | (unchanged) | (unchanged) |
| ThreadID | `thread_id` | *int64 | nil (unset) | if set, > 0. Otherwise validation fails: `notify.telegram.thread_id: must be a positive integer` |
| Silent | `silent` | bool | false | none beyond the type |

## telegram.Notifier settings

- `threadID int64`: 0 means no topic. It comes from `WithThread` and is set only when config has a value.
- `silent bool`: from `WithSilent`.
- `logger *slog.Logger`: from `WithLogger`. The default discards output.

## sendMessageRequest (wire)

| JSON | When present |
|---|---|
| `text`, `chat_id`, `link_preview_options` | always (unchanged) |
| `message_thread_id` | the thread is configured and this chat's topic switch is on |
| `disable_notification` | `silent` is true (value `true`) |

## Per-chat delivery state (one `Send`, not persisted)

```text
useThread = (threadID != 0)
for each part:
    send(part, useThread)
    on threadNotFound (only possible while useThread):
        useThread = false; WARN once; send(part, false) via the ordinary retry loop
    on any other failure: stop this chat (unchanged)
```

Transitions: `useThread` goes true → false at most once per chat per run, and never back.

## Attempt outcome (`classify`)

The existing outcomes are delivered, retry-after-wait and fail. A new one is added: **threadNotFound** (fail,
not retried). It is produced only when the reply failed and its `description` contains `message thread not
found` (case-insensitive), and the caller treats it as a fallback trigger only when the request carried a thread
id. Its error text is the HTTP status, as today, never the description.
