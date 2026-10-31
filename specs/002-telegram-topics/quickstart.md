# Quickstart: Telegram Forum Topic and Silent Delivery

## Automated (part of `task check`)

```sh
task check
```

It must cover, with per-client `httpmock` transports and anonymized fixtures:

1. Neither key set: the recorded request JSON equals today's (no `message_thread_id`, no `disable_notification`).
2. `thread_id` + `silent`: every part to every chat carries both.
3. Thread not found on part 1 of 3: part 1 is resent without the topic. Parts 2 and 3 go without it. One WARN.
   The WARN has the masked chat and thread id and no description text.
4. Two chats, only one rejects the topic: only that chat falls back.
5. Closed topic (400, other description): no fallback, the chat fails as today.
6. Fallback resend fails (e.g. 500 then exhausted): the chat fails under today's retry rules.
7. Config: `thread_id: 0`, `-1` → validation error naming the key. `thread_id: "x"`, `silent: "yes"` → load error.

## Live check (manual, owner)

Prerequisites: the forum supergroup exists, the bot is a member, and the Finance topic id is known.

1. Set `chat_ids: [<group id>]`, `thread_id: <finance id>`, `silent: true`.
2. Force a digest (e.g. a recent bank entry not yet in Firefly III), then run `firefly-jar check`.
   Expect: the digest appears in Finance with no notification sound. The exit code is 1.
3. Set `thread_id` to a non-existent id and run again. Expect: the digest lands in General, and cron or stderr
   shows one WARN `telegram topic not found, sent to general`. The exit code is still 1.
4. Remove both keys. Expect: exactly today's behaviour.
