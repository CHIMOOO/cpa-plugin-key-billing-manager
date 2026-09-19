// Package messages attaches stable translation metadata at message creation.
// It never attempts to interpret user data or match rendered error sentences.
package messages

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed catalog.json
var catalogJSON []byte

type catalogEntry struct {
	Key     string   `json:"key"`
	Formats []string `json:"formats"`
}

var catalog = func() map[string]catalogEntry {
	var entries map[string]catalogEntry
	if err := json.Unmarshal(catalogJSON, &entries); err != nil {
		panic(err)
	}
	return entries
}()

type Message struct {
	Key    string            `json:"message_key,omitempty"`
	Params map[string]string `json:"message_params,omitempty"`
	Text   string            `json:"-"`
}

func (m Message) IsZero() bool { return m.Key == "" }

// New accepts a source template and its arguments, before formatting loses context.
func New(format string, args ...any) Message {
	text := format
	if len(args) > 0 {
		text = fmt.Sprintf(strings.ReplaceAll(format, "%w", "%v"), args...)
	}
	result := Message{Text: text}
	entry, ok := catalog[format]
	if !ok || len(entry.Formats) != len(args) {
		return result
	}
	result.Key = entry.Key
	if len(args) > 0 {
		result.Params = make(map[string]string, len(args))
		for i, arg := range args {
			result.Params[fmt.Sprintf("v%d", i)] = fmt.Sprintf(entry.Formats[i], arg)
		}
	}
	return result
}

// Literal describes a known, unformatted source message without treating raw
// diagnostics containing percent signs as formatting instructions.
func Literal(text string) Message { return New(text, []any{}...) }

type describedError struct {
	error
	detail Message
}

func (e *describedError) MessageDetail() Message { return e.detail }
func (e *describedError) Unwrap() error          { return e.error }

func Errorf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	detail := New(format, args...)
	detail.Text = err.Error()
	return &describedError{error: err, detail: detail}
}

func FromError(err error) Message {
	if err == nil {
		return Message{}
	}
	// Only the error that owns the full message can supply its translation.
	// Looking through arbitrary wrappers would lose their context; matching raw
	// upstream text against the catalog could translate user-provided content.
	if described, ok := err.(interface{ MessageDetail() Message }); ok {
		return described.MessageDetail()
	}
	return Message{Text: err.Error()}
}
