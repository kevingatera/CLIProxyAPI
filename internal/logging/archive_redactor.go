package logging

import (
	"bytes"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/MoeForge/moego/regexp"
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

// NewArchiveRedactor builds a redactor for the given exact secret values. Each
// value is also matched in its URL-escaped and JSON-escaped forms.
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

// errArchiveEdit reports a JSON path that cannot be edited safely.
var errArchiveEdit = errors.New("archive: JSON path cannot be edited")

// archiveSetPath replaces the value at path in doc. The path must resolve to an
// existing value, so an escaped key cannot make the editor add a new member next
// to an unmasked original.
func archiveSetPath(doc *ojson.Doc, path, value string) error {
	if path == "" || !doc.Get(path).Exists() {
		return errArchiveEdit
	}
	return doc.Set(path, value)
}

// archiveJoin appends one already escaped path component.
func archiveJoin(parent, comp string) string {
	if parent == "" {
		return comp
	}
	return parent + "." + comp
}

// splitArchiveDataPrefix separates an SSE "data:" prefix from a line.
func splitArchiveDataPrefix(line string) (string, string) {
	switch {
	case strings.HasPrefix(line, "data: "):
		return "data: ", strings.TrimPrefix(line, "data: ")
	case strings.HasPrefix(line, "data:"):
		return "data:", strings.TrimPrefix(line, "data:")
	}
	return "", line
}

// Transcript also masks a credential emitted across separate JSON delta
// records, in addition to credentials split across transport chunks. Each record
// is edited through its own ojson.Doc, so untouched fields keep their order and
// number formatting.
func (r *ArchiveRedactor) Transcript(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	type record struct {
		doc     *ojson.Doc
		prefix  string
		changed bool
		failed  bool
	}
	type span struct {
		text       string
		start, end int
		record     int
		path       string
	}
	records := make(map[int]*record)
	lanes := make(map[string][]span)
	lengths := make(map[string]int)
	for index, line := range lines {
		prefix, body := splitArchiveDataPrefix(line)
		if !ojson.Valid(body) {
			continue
		}
		rec := &record{doc: ojson.NewDocString(body), prefix: prefix}
		records[index] = rec
		var visit func(ojson.Result, string)
		visit = func(v ojson.Result, path string) {
			switch {
			case v.IsObject():
				seen := make(map[string]bool)
				v.ForEach(func(key, item ojson.Result) bool {
					name := key.String()
					if seen[name] {
						// A duplicate key cannot be addressed by path, so the record fails closed.
						rec.failed = true
						return false
					}
					seen[name] = true
					childPath := archiveJoin(path, ojson.Escape(name))
					if archiveTextField(name) && item.Type == ojson.String {
						text := item.String()
						start := lengths[name]
						lengths[name] += len(text)
						lanes[name] = append(lanes[name], span{text: text, start: start, end: lengths[name], record: index, path: childPath})
						return true
					}
					visit(item, childPath)
					return true
				})
			case v.IsArray():
				for i, item := range v.Array() {
					visit(item, archiveJoin(path, strconv.Itoa(i)))
				}
			}
		}
		visit(ojson.Parse(body), "")
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
					rec := records[s.record]
					if err := archiveSetPath(rec.doc, s.path, archiveRedacted); err != nil {
						rec.failed = true
					} else {
						rec.changed = true
					}
					break
				}
			}
		}
	}
	for index, rec := range records {
		switch {
		case rec.failed:
			lines[index] = rec.prefix + archiveRedacted
		case rec.changed:
			lines[index] = rec.prefix + string(rec.doc.Bytes())
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

// Bytes masks one section. Known secrets are replaced first, then JSON fields
// with sensitive names, then text patterns.
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
	if out, changed := r.redactJSONDocument(s); changed {
		return out
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		prefix, body := splitArchiveDataPrefix(line)
		if out, changed := r.redactJSONDocument(body); changed {
			lines[i] = prefix + out
		}
	}
	return strings.Join(lines, "\n")
}

// redactJSONDocument masks sensitive fields and known secrets in one JSON object or
// array. It reports changed=false when text is not such a document or needs no edits.
// If an edit cannot be applied safely, the whole document is replaced by [REDACTED].
func (r *ArchiveRedactor) redactJSONDocument(text string) (string, bool) {
	if !ojson.Valid(text) {
		return text, false
	}
	root := ojson.Parse(text)
	if !root.IsObject() && !root.IsArray() {
		return text, false
	}
	e := &archiveEdit{r: r, doc: ojson.NewDocString(text)}
	e.walk(root, "")
	switch {
	case e.failed:
		return archiveRedacted, true
	case e.changed:
		return string(e.doc.Bytes()), true
	}
	return text, false
}

// archiveEdit applies masking edits to one document. Every edit is a path set on
// ojson.Doc, so untouched bytes are preserved exactly.
type archiveEdit struct {
	r       *ArchiveRedactor
	doc     *ojson.Doc
	changed bool
	failed  bool
}

// set replaces the value at path. An edit that cannot be applied marks the document failed.
func (e *archiveEdit) set(path, value string) {
	if e.failed {
		return
	}
	if err := archiveSetPath(e.doc, path, value); err != nil {
		e.failed = true
		return
	}
	e.changed = true
}

// walk visits the members of the object or array v, which sits at path.
func (e *archiveEdit) walk(v ojson.Result, path string) {
	switch {
	case v.IsObject():
		seen := make(map[string]bool)
		v.ForEach(func(key, item ojson.Result) bool {
			name := key.String()
			if seen[name] {
				// A duplicate key cannot be addressed by path, so the document fails closed.
				e.failed = true
				return false
			}
			seen[name] = true
			e.value(item, archiveJoin(path, ojson.Escape(name)), name)
			return !e.failed
		})
	case v.IsArray():
		for i, item := range v.Array() {
			e.value(item, archiveJoin(path, strconv.Itoa(i)), "")
		}
	}
}

// value masks one member. name is the object key, empty for array items.
func (e *archiveEdit) value(v ojson.Result, path, name string) {
	if archiveSensitiveName(name) {
		e.set(path, archiveRedacted)
		return
	}
	if v.Type == ojson.String {
		text := v.String()
		// Some clients send JSON as escaped text, so look inside string values too.
		if inner, changed := e.r.redactJSONDocument(text); changed {
			e.set(path, inner)
			return
		}
		if safe := e.r.replacements.Replace(text); safe != text {
			e.set(path, safe)
		}
		return
	}
	e.walk(v, path)
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

// Headers masks sensitive header names outright and scans the rest for secrets.
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
