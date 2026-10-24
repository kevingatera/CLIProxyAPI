package helps

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
)

// CursorProviderModelID strips the provider prefix and any route path from a model name.
func CursorProviderModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(model), "cursor/") {
		model = strings.TrimSpace(model[len("cursor/"):])
	}
	if strings.Contains(model, "/") {
		parts := strings.Split(model, "/")
		model = strings.TrimSpace(parts[len(parts)-1])
	}
	return model
}

// CursorPromptFromOpenAIPayload flattens an OpenAI chat payload into one text prompt.
func CursorPromptFromOpenAIPayload(payload []byte) string {
	root := gjson.ParseBytes(payload)
	segments := make([]string, 0, 16)
	root.Get("messages").ForEach(func(_, message gjson.Result) bool {
		role := strings.TrimSpace(message.Get("role").String())
		if role == "" {
			role = "user"
		}
		if text := cursorExtractMessageContent(message); text != "" {
			segments = append(segments, strings.ToUpper(role)+":\n"+text)
		}
		return true
	})
	if len(segments) > 0 {
		return strings.TrimSpace(strings.Join(segments, "\n\n"))
	}

	// No message text: fall back to tool names and any raw input or prompt field.
	fallback := make([]string, 0, 8)
	root.Get("tools").ForEach(func(_, tool gjson.Result) bool {
		if name := tool.Get("function.name").String(); name != "" {
			fallback = append(fallback, name)
		}
		return true
	})
	root.Get("functions").ForEach(func(_, fn gjson.Result) bool {
		if name := fn.Get("name").String(); name != "" {
			fallback = append(fallback, name)
		}
		return true
	})
	cursorAddIfNotEmpty(&fallback, root.Get("input").String())
	cursorAddIfNotEmpty(&fallback, root.Get("prompt").String())
	return strings.TrimSpace(strings.Join(fallback, "\n"))
}

// CursorPromptFromOpenAIPayloadWithImages builds the prompt and writes any data-URL
// images into dir. The prompt lists the written files so the agent can open them.
// dir must be the request's private workspace.
func CursorPromptFromOpenAIPayloadWithImages(payload []byte, dir string) (string, error) {
	prompt := CursorPromptFromOpenAIPayload(payload)
	attachments, err := materializeCursorImages(payload, dir)
	if err != nil {
		return "", err
	}
	if len(attachments) == 0 {
		return prompt, nil
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(prompt))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString("Attached image files for this request:\n")
	for _, path := range attachments {
		b.WriteString("- ")
		b.WriteString(path)
		b.WriteString("\n")
	}
	b.WriteString("Inspect the local image files when answering.")
	return strings.TrimSpace(b.String()), nil
}

func cursorAddIfNotEmpty(segments *[]string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	*segments = append(*segments, value)
}

func cursorExtractMessageContent(message gjson.Result) string {
	content := cursorFlattenContent(message.Get("content"))
	if content == "" {
		return ""
	}
	name := strings.TrimSpace(message.Get("name").String())
	if name == "" {
		return content
	}
	return fmt.Sprintf("name=%s\n%s", name, content)
}

func cursorFlattenContent(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	switch content.Type {
	case gjson.String:
		return strings.TrimSpace(content.String())
	case gjson.JSON:
		if content.IsArray() {
			parts := make([]string, 0, len(content.Array()))
			content.ForEach(func(_, item gjson.Result) bool {
				if flat := cursorFlattenContent(item); flat != "" {
					parts = append(parts, flat)
				}
				return true
			})
			return strings.TrimSpace(strings.Join(parts, "\n"))
		}
		if text := strings.TrimSpace(content.Get("text").String()); text != "" {
			return text
		}
		if content.Get("type").String() == "image_url" {
			if url := strings.TrimSpace(content.Get("image_url.url").String()); url != "" {
				return cursorImageReferenceText(url)
			}
		}
		if content.Get("type").String() == "input_image" {
			if url := strings.TrimSpace(content.Get("image_url").String()); url != "" {
				return cursorImageReferenceText(url)
			}
		}
		return strings.TrimSpace(content.Raw)
	default:
		return strings.TrimSpace(content.String())
	}
}

func cursorImageReferenceText(imageURL string) string {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return ""
	}
	if cursorIsImageDataURL(imageURL) {
		return "[image attached]"
	}
	return "[image] " + imageURL
}

// materializeCursorImages writes each distinct image data URL in payload to dir (mode 0600)
// and returns the file paths in first-seen order.
func materializeCursorImages(payload []byte, dir string) ([]string, error) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, nil
	}
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, nil
	}
	var paths []string
	seen := make(map[string]string)
	var walk func(any) error
	walk = func(value any) error {
		switch v := value.(type) {
		case map[string]any:
			for _, child := range v {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range v {
				if err := walk(child); err != nil {
					return err
				}
			}
		case string:
			imageURL := strings.TrimSpace(v)
			if !cursorIsImageDataURL(imageURL) {
				return nil
			}
			if path, ok := seen[imageURL]; ok {
				paths = append(paths, path)
				return nil
			}
			mediaType, data, err := decodeCursorImageDataURL(imageURL)
			if err != nil {
				return err
			}
			path := filepath.Join(dir, fmt.Sprintf("image-%d%s", len(seen)+1, cursorImageExtension(mediaType)))
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return fmt.Errorf("cursor executor: write image attachment: %w", err)
			}
			seen[imageURL] = path
			paths = append(paths, path)
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return paths, nil
}

func cursorIsImageDataURL(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "data:image/")
}

func decodeCursorImageDataURL(value string) (string, []byte, error) {
	value = strings.TrimSpace(value)
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return "", nil, CursorStatusError{Code: http.StatusBadRequest, Msg: "cursor executor: malformed image data URL"}
	}
	meta := strings.TrimSpace(value[len("data:"):comma])
	dataPart := strings.TrimSpace(value[comma+1:])
	mediaType := meta
	if semi := strings.IndexByte(mediaType, ';'); semi >= 0 {
		mediaType = mediaType[:semi]
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return "", nil, CursorStatusError{Code: http.StatusBadRequest, Msg: "cursor executor: data URL is not an image"}
	}
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return "", nil, CursorStatusError{Code: http.StatusBadRequest, Msg: "cursor executor: image data URL must be base64 encoded"}
	}
	data, err := base64.StdEncoding.DecodeString(dataPart)
	if err != nil {
		return "", nil, CursorStatusError{Code: http.StatusBadRequest, Msg: "cursor executor: invalid image base64 data"}
	}
	return strings.ToLower(mediaType), data, nil
}

func cursorImageExtension(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	default:
		return ".png"
	}
}
