# Contract delta: `notify.telegram`

This extends `specs/001-missing-tx-reminder/contracts/config.md`. Implementation folds the delta into that file
(the living config contract) and into `config.example.yaml`.

```yaml
notify:
  telegram:
    bot_token_file: /etc/firefly-jar/telegram.token   # or env FIREFLY_JAR_TELEGRAM_TOKEN
    chat_ids: [-1001234567890]
    thread_id: 42        # optional: forum topic (message_thread_id) for every chat; unset = no topic
    silent: true         # optional, default false: deliver without a notification sound
```

## Rules

- `thread_id`: optional. If present, it must be an integer > 0, or validation fails (exit 2) with
  `notify.telegram.thread_id: must be a positive integer`. A non-integer fails loading.
- `silent`: optional boolean, default false. A non-boolean fails loading.
- The two are independent. Neither implies the other.
- Neither set: the Telegram request is identical to the previous release's.

## Delivery behaviour

- If Telegram rejects a message because its topic was not found, firefly-jar resends that message once without
  the topic (it lands in General), sends that chat's remaining parts without it too, and logs one WARN per chat.
  Cron mails the WARN. The next run tries the topic again.
- A closed topic, and any other error, follows the existing delivery-failure rules (FR-028, FR-029).
