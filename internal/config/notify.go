package config

// Notify is the notify: section. A nil channel is not configured. Whether at least one recipient
// exists is checked only by ValidateFor(CmdCheck, false), because check --stdout needs none.
type Notify struct {
	Telegram *Telegram `yaml:"telegram"`
	Email    *Email    `yaml:"email"`
}

// Telegram is the notify.telegram: channel. ThreadID is the forum topic used for every chat in
// ChatIDs (nil means no topic, so the digest goes to the chat itself). Silent sends the digest
// without a notification sound (Telegram's disable_notification).
type Telegram struct {
	BotTokenFile string  `yaml:"bot_token_file"`
	ChatIDs      []int64 `yaml:"chat_ids"`
	ThreadID     *int64  `yaml:"thread_id"`
	Silent       bool    `yaml:"silent"`
}

// Email is the notify.email: channel. Port is 587 (STARTTLS required) or 465 (implicit TLS).
type Email struct {
	Host         string   `yaml:"host"`
	Port         int      `yaml:"port"`
	Username     string   `yaml:"username"`
	PasswordFile string   `yaml:"password_file"`
	From         string   `yaml:"from"`
	To           []string `yaml:"to"`
}
