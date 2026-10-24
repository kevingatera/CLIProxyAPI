package logging

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const archiveRedacted = "[REDACTED]"

var archivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(["']?(?:access[\s_-]*token|refresh[\s_-]*token|id[\s_-]*token|api[\s_-]*key|client[\s_-]*secret|private[\s_-]*key|proxy[\s_-]*authorization|authorization|password|passwd|passphrase|credential|token|secret|x-api-key|x-goog-api-key|key|cookie|set-cookie)["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;&}\]]+)`),
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[^\s,"'\\;<>]+`),
	regexp.MustCompile(`(?i)(?:sk-(?:ant-)?[a-z0-9_-]{12,}|gh[pousr]_[a-z0-9]{20,}|github_pat_[a-z0-9_]{20,}|AIza[a-z0-9_-]{25,}|xox[baprs]-[a-z0-9-]{12,}|eyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+)`),
	regexp.MustCompile(`(?m)(^Auth:.*?\bvalue=)[^\n]+`),
}

var archiveHeaderSecret = regexp.MustCompile(`(?im)^((?:authorization|proxy-authorization|x-api-key|x-goog-api-key|cookie|set-cookie):)[^\n]*`)

var archiveIncompletePrivateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*`)

var archivePrivateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

// ArchiveRedactor masks known gateway credentials, credential fields, and
// recognizable secret formats without changing the traffic sent to clients.
type ArchiveRedactor struct {
	replacements *strings.Replacer
	secrets      []string
}

func NewArchiveRedactor(secrets []string) *ArchiveRedactor {
	values := make(map[string]bool)
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		values[secret] = true
		values[url.QueryEscape(secret)] = true
		encoded, _ := json.Marshal(secret)
		if len(encoded) > 2 {
			values[string(encoded[1:len(encoded)-1])] = true
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	pairs := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		pairs = append(pairs, key, archiveRedacted)
	}
	return &ArchiveRedactor{replacements: strings.NewReplacer(pairs...), secrets: keys}
}

// Transcript also masks a credential emitted across separate JSON delta
// records, in addition to credentials split across transport chunks.
func (r *ArchiveRedactor) Transcript(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	type record struct {
		value   any
		prefix  string
		changed bool
	}
	type span struct {
		text       string
		start, end int
		record     int
		set        func()
	}
	records := make(map[int]*record)
	lanes := make(map[string][]span)
	lengths := make(map[string]int)
	for index, line := range lines {
		prefix := ""
		if strings.HasPrefix(line, "data:") {
			prefix = "data:"
			if strings.HasPrefix(line, "data: ") {
				prefix = "data: "
			}
			line = strings.TrimPrefix(line, prefix)
		}
		var value any
		if json.Unmarshal([]byte(line), &value) != nil {
			continue
		}
		records[index] = &record{value: value, prefix: prefix}
		var visit func(any)
		visit = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				keys := make([]string, 0, len(x))
				for k := range x {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, key := range keys {
					item := x[key]
					text, ok := item.(string)
					if ok && archiveTextField(key) {
						start := lengths[key]
						lengths[key] += len(text)
						lanes[key] = append(lanes[key], span{text: text, start: start, end: lengths[key], record: index, set: func() { x[key] = archiveRedacted }})
					} else {
						visit(item)
					}
				}
			case []any:
				for _, item := range x {
					visit(item)
				}
			}
		}
		visit(value)
	}
	for _, spans := range lanes {
		var joined strings.Builder
		for _, s := range spans {
			joined.WriteString(s.text)
		}
		text := joined.String()
		var matches [][2]int
		for _, secret := range r.secrets {
			for offset := 0; offset < len(text); {
				i := strings.Index(text[offset:], secret)
				if i < 0 {
					break
				}
				start := offset + i
				matches = append(matches, [2]int{start, start + len(secret)})
				offset = start + len(secret)
			}
		}
		for _, pattern := range append(append([]*regexp.Regexp{}, archivePatterns...), archivePrivateKey) {
			for _, match := range pattern.FindAllStringIndex(text, -1) {
				matches = append(matches, [2]int{match[0], match[1]})
			}
		}
		for _, s := range spans {
			for _, match := range matches {
				if s.start < match[1] && s.end > match[0] {
					s.set()
					records[s.record].changed = true
					break
				}
			}
		}
	}
	for index, record := range records {
		if record.changed {
			encoded, _ := json.Marshal(record.value)
			lines[index] = record.prefix + string(encoded)
		}
	}
	return r.Bytes([]byte(strings.Join(lines, "\n")))
}

func archiveTextField(key string) bool {
	switch key {
	case "text", "content", "delta", "arguments", "partial_json", "thinking", "reasoning_content":
		return true
	}
	return false
}

func (r *ArchiveRedactor) Bytes(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	// Binary bodies cannot be inspected reliably for embedded credentials.
	if bytes.IndexByte(data, 0) >= 0 {
		return []byte("[BINARY PAYLOAD OMITTED]\n")
	}
	s := r.replacements.Replace(string(data))
	s = r.redactArchiveJSON(s)
	s = archivePrivateKey.ReplaceAllString(s, archiveRedacted)
	s = archiveIncompletePrivateKey.ReplaceAllString(s, archiveRedacted)
	s = urlUserinfoLogPattern.ReplaceAllString(s, `${1}[REDACTED]@`)
	s = archivePatterns[0].ReplaceAllString(s, `${1}"[REDACTED]"`)
	s = archivePatterns[1].ReplaceAllString(s, archiveRedacted)
	s = archivePatterns[2].ReplaceAllString(s, archiveRedacted)
	s = archivePatterns[3].ReplaceAllString(s, `${1}[REDACTED]`)
	s = archiveHeaderSecret.ReplaceAllString(s, `${1} [REDACTED]`)
	return []byte(s)
}

func (r *ArchiveRedactor) redactArchiveJSON(s string) string {
	var value any
	if json.Unmarshal([]byte(s), &value) == nil {
		if masked, known := maskArchiveFields(value), r.maskKnownArchiveValues(value); masked || known {
			encoded, _ := json.Marshal(value)
			return string(encoded)
		}
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		prefix := ""
		if strings.HasPrefix(line, "data:") {
			prefix = "data:"
			if strings.HasPrefix(line, "data: ") {
				prefix = "data: "
			}
			line = strings.TrimPrefix(line, prefix)
		}
		if json.Unmarshal([]byte(line), &value) == nil {
			if masked, known := maskArchiveFields(value), r.maskKnownArchiveValues(value); !(masked || known) {
				continue
			}
			encoded, _ := json.Marshal(value)
			lines[i] = prefix + string(encoded)
		}
	}
	return strings.Join(lines, "\n")
}

func maskArchiveFields(value any) bool {
	changed := false
	switch x := value.(type) {
	case map[string]any:
		for key, v := range x {
			if archiveSensitiveName(key) {
				x[key] = archiveRedacted
				changed = true
			} else if maskArchiveFields(v) {
				changed = true
			} else if text, ok := v.(string); ok {
				var nested any
				if json.Unmarshal([]byte(text), &nested) == nil && maskArchiveFields(nested) {
					encoded, _ := json.Marshal(nested)
					x[key] = string(encoded)
					changed = true
				}
			}
		}
	case []any:
		for _, v := range x {
			if maskArchiveFields(v) {
				changed = true
			}
		}
	}
	return changed
}

func archiveSensitiveName(name string) bool {
	n := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(name))
	switch n {
	case "apikey", "apikeys", "accesstoken", "refreshtoken", "idtoken", "token", "secret", "secretkey", "clientsecret", "privatekey", "password", "passwd", "passphrase", "authorization", "proxyauthorization", "cookie", "setcookie", "credential", "credentials", "xapikey", "xgoogapikey":
		return true
	}
	return strings.HasSuffix(n, "password") || strings.HasSuffix(n, "secret") || strings.HasSuffix(n, "apikey") || strings.HasSuffix(n, "token")
}

// ArchiveSecrets extracts credential values from a JSON-compatible config or
// runtime auth snapshot. It does not retain the rest of the snapshot.
func ArchiveSecrets(value any) []string {
	b, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var decoded any
	if json.Unmarshal(b, &decoded) != nil {
		return nil
	}
	var out []string
	var visit func(any, bool)
	visit = func(v any, sensitive bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				visit(item, sensitive || archiveSensitiveName(k))
			}
		case []any:
			for _, item := range x {
				visit(item, sensitive)
			}
		case string:
			if sensitive {
				out = append(out, x)
			}
		}
	}
	visit(decoded, false)
	return out
}

func (r *ArchiveRedactor) Headers(headers map[string][]string) map[string][]string {
	out := make(map[string][]string, len(headers))
	for name, values := range headers {
		for _, value := range values {
			if archiveSensitiveName(name) {
				value = archiveRedacted
			} else {
				value = string(r.Bytes([]byte(value)))
			}
			out[name] = append(out[name], value)
		}
	}
	return out
}

// Decode string escapes before matching exact credential values, so JSON
// unicode escapes cannot hide a configured key from the archive masker.
func (r *ArchiveRedactor) maskKnownArchiveValues(value any) bool {
	changed := false
	switch x := value.(type) {
	case map[string]any:
		for key, item := range x {
			if text, ok := item.(string); ok {
				safe := r.replacements.Replace(text)
				if safe != text {
					x[key] = safe
					changed = true
				}
			} else if r.maskKnownArchiveValues(item) {
				changed = true
			}
		}
	case []any:
		for i, item := range x {
			if text, ok := item.(string); ok {
				safe := r.replacements.Replace(text)
				if safe != text {
					x[i] = safe
					changed = true
				}
			} else if r.maskKnownArchiveValues(item) {
				changed = true
			}
		}
	}
	return changed
}
