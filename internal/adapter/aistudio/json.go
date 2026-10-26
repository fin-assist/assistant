package aistudio

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// decodeJSON rejects duplicate members before typed decoding so conflicting provider
// fields cannot be interpreted differently by the stream and completion consumers.
func decodeJSON(data []byte, target any) error {
	return decodeJSONPolicy(data, target, false)
}

// AI Studio emits duplicate identical boolean validity flags in response tool metadata.
// This exception never applies to request JSON or any conflicting/significant member.
func decodeProviderJSON(data []byte, target any) error {
	return decodeJSONPolicy(data, target, true)
}

func decodeJSONPolicy(data []byte, target any, providerFlags bool) error {
	if !utf8.Valid(data) {
		return errors.New("invalid JSON encoding")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := jsonValue(d, data, providerFlags); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON boundary")
	}
	typed := json.NewDecoder(bytes.NewReader(data))
	typed.UseNumber()
	return typed.Decode(target)
}
func jsonValue(d *json.Decoder, data []byte, providerFlags bool) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]bool)
		var validity string
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok || (keys[s] && (!providerFlags || s != "valid")) {
				return errors.New("invalid JSON member")
			}
			start := d.InputOffset()
			if err := jsonValue(d, data, providerFlags); err != nil {
				return err
			}
			if s == "valid" && providerFlags {
				value := string(bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(data[start:d.InputOffset()]), []byte(":"))))
				if keys[s] && (value != validity || (value != "true" && value != "false")) {
					return errors.New("invalid JSON member")
				}
				validity = value
			}
			keys[s] = true
		}
	case '[':
		for d.More() {
			if err := jsonValue(d, data, providerFlags); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	end, err := d.Token()
	if err != nil {
		return err
	}
	if delim == '{' && end != json.Delim('}') || delim == '[' && end != json.Delim(']') {
		return errors.New("invalid JSON boundary")
	}
	return nil
}
