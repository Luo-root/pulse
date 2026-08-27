package demoapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luo-root/pulse/llm"
)

// Attachment 是用户媒体附件，最终映射为 llm.Part。
type Attachment struct {
	MediaType string
	Data      []byte
	URL       string
}

// Input 是 demo 侧的用户输入。示例不解析任何供应商线格式。
type Input struct {
	Text        string
	ImageURL    string
	ImageType   string
	ImageFile   string
	MediaURL    string
	MediaType   string
	MediaFile   string
	Attachments []Attachment
}

// Message 把输入转成一条带类型化内容块的用户消息。
func (in Input) Message() (*llm.Message, error) {
	parts, err := in.Parts()
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("demo: empty user input")
	}
	return llm.User(parts...), nil
}

// Parts 把输入转成 llm.Part 列表。
func (in Input) Parts() ([]llm.Part, error) {
	var parts []llm.Part
	if strings.TrimSpace(in.Text) != "" {
		parts = append(parts, llm.Text(in.Text))
	}
	if in.ImageURL != "" {
		mediaType := in.ImageType
		if mediaType == "" {
			mediaType = "image/png"
		}
		parts = append(parts, llm.ImageURL(in.ImageURL, mediaType))
	}
	if in.ImageFile != "" {
		data, mediaType, err := readMedia(in.ImageFile, in.ImageType)
		if err != nil {
			return nil, err
		}
		parts = append(parts, llm.ImageData(mediaType, data))
	}
	if in.MediaURL != "" {
		mediaType := in.MediaType
		if mediaType == "" {
			mediaType = guessMediaType(in.MediaURL)
		}
		parts = append(parts, llm.MediaURL(mediaType, in.MediaURL))
	}
	if in.MediaFile != "" {
		data, mediaType, err := readMedia(in.MediaFile, in.MediaType)
		if err != nil {
			return nil, err
		}
		parts = append(parts, llm.Media(mediaType, data))
	}
	for _, item := range in.Attachments {
		switch {
		case item.URL != "" && strings.HasPrefix(item.MediaType, "image/"):
			parts = append(parts, llm.ImageURL(item.URL, item.MediaType))
		case len(item.Data) > 0 && strings.HasPrefix(item.MediaType, "image/"):
			parts = append(parts, llm.ImageData(item.MediaType, item.Data))
		case item.URL != "":
			parts = append(parts, llm.MediaURL(item.MediaType, item.URL))
		case len(item.Data) > 0:
			parts = append(parts, llm.Media(item.MediaType, item.Data))
		}
	}
	return parts, nil
}

func readMedia(path, mediaType string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("demo: read %s: %w", path, err)
	}
	if mediaType == "" {
		mediaType = guessMediaType(path)
	}
	return data, mediaType, nil
}

func guessMediaType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".pdf":
		return "application/pdf"
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".mp4":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

// ExtractText 拼接用户消息中的文本块，供检索查询使用。
func ExtractText(messages []*llm.Message) string {
	var b strings.Builder
	for _, message := range messages {
		if message == nil {
			continue
		}
		if t := strings.TrimSpace(message.Text()); t != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t)
		}
	}
	return b.String()
}
