package config

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultOffTopicText is the server-owned explanation for an off-topic question.
const DefaultOffTopicText = "Вопрос не по теме — помощник отвечает только на вопросы о ваших финансах. С другими вопросами помогут универсальные ассистенты: Алиса AI, GigaChat, ChatGPT, Claude, DeepSeek"

// OffTopicConfig bounds free off-topic attempts over a rolling window.
type OffTopicConfig struct {
	Max    int
	Window time.Duration
	Text   string
}

// DefaultOffTopicConfig supplies the same defaults to direct assembly and env loading.
func DefaultOffTopicConfig() OffTopicConfig {
	return OffTopicConfig{Max: 5, Window: 168 * time.Hour, Text: DefaultOffTopicText}
}

func (r *reader) offTopic() OffTopicConfig {
	return OffTopicConfig{Max: r.int("ASSISTANT_OFF_TOPIC_MAX"), Window: r.duration("ASSISTANT_OFF_TOPIC_WINDOW"), Text: strings.TrimSpace(r.str("ASSISTANT_OFF_TOPIC_TEXT"))}
}
func (c Config) validateOffTopic() []error {
	var errs []error
	if c.OffTopic.Max < 1 || c.OffTopic.Max > 32767 {
		errs = append(errs, errors.New("ASSISTANT_OFF_TOPIC_MAX must be between 1 and 32767"))
	}
	if c.OffTopic.Window < time.Millisecond || c.OffTopic.Window%time.Millisecond != 0 {
		errs = append(errs, errors.New("ASSISTANT_OFF_TOPIC_WINDOW must be a positive whole number of milliseconds"))
	}
	if strings.TrimSpace(c.OffTopic.Text) == "" || !utf8.ValidString(c.OffTopic.Text) || len(c.OffTopic.Text) > 4096 {
		errs = append(errs, errors.New("ASSISTANT_OFF_TOPIC_TEXT must be nonempty UTF-8 of at most 4096 bytes"))
	}
	return errs
}
