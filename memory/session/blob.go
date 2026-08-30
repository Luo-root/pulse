package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Luo-root/pulse/llm"
)

// blobInlineLimit 是 llm.Part 内联字节的原样内联上限（JSON base64 形态）；
// 超限字节写入 {session}/blobs/{sha256}，Part 换稳定引用。
const blobInlineLimit = 32 * 1024 // 32KiB

// blobURLPrefix 是 Part 引用形态的 URL 前缀（稳定引用，跨会话可读）。
const blobURLPrefix = "blob:"

// blobRef 描述一个 Part 字段从内联字节换成 blob 引用后的形态。
type blobRef struct {
	sha string
}

// encodeBlobs 把 message 事件 payload 中超限的内联字节替换为 blob 引用并
// 落盘（内容寻址：同字节同 sha，已存在则去重复用）。非 message 类型或
// 不含超限字节的 payload 原样返回。调用方保证 blobsDir 可写。
//
// 禁止静默丢字节：超限字节必须完整写入 blob 文件，否则返回错误。
func encodeBlobs(payload json.RawMessage, blobsDir string) (json.RawMessage, error) {
	if len(payload) == 0 {
		return payload, nil
	}
	var p MessagePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		// 只对 message.* 做 blob 替换；其它 payload 原样（调用方限定类型）。
		return payload, nil
	}
	changed := false
	for i := range p.Parts {
		part := &p.Parts[i]
		switch {
		case part.Image != nil && len(part.Image.Data) > blobInlineLimit:
			sha, err := writeBlob(blobsDir, part.Image.Data)
			if err != nil {
				return nil, err
			}
			part.Image = &llm.ImageSource{URL: blobURLPrefix + sha, MediaType: part.Image.MediaType}
			changed = true
		case part.Media != nil && len(part.Media.Data) > blobInlineLimit:
			sha, err := writeBlob(blobsDir, part.Media.Data)
			if err != nil {
				return nil, err
			}
			part.Media = &llm.MediaContent{URL: blobURLPrefix + sha, MediaType: part.Media.MediaType, Metadata: part.Media.Metadata}
			changed = true
		}
	}
	if !changed {
		return payload, nil
	}
	out, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("%w: re-encode payload with blob refs: %v", ErrPayloadInvalid, err)
	}
	return out, nil
}

// decodeBlobs 是 encodeBlobs 的逆操作：把 blob 引用还原为内联字节。
// 引用指向的 blob 缺失 = 加载错误（fail closed），不降级为空内容。
func decodeBlobs(payload json.RawMessage, blobsDir string) (json.RawMessage, error) {
	if len(payload) == 0 {
		return payload, nil
	}
	var p MessagePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return payload, nil
	}
	changed := false
	for i := range p.Parts {
		part := &p.Parts[i]
		switch {
		case part.Image != nil && isBlobRef(part.Image.URL):
			sha := part.Image.URL[len(blobURLPrefix):]
			data, err := readBlob(blobsDir, sha)
			if err != nil {
				return nil, err
			}
			part.Image = &llm.ImageSource{Data: data, MediaType: part.Image.MediaType}
			changed = true
		case part.Media != nil && isBlobRef(part.Media.URL):
			sha := part.Media.URL[len(blobURLPrefix):]
			data, err := readBlob(blobsDir, sha)
			if err != nil {
				return nil, err
			}
			part.Media = &llm.MediaContent{Data: data, MediaType: part.Media.MediaType, Metadata: part.Media.Metadata}
			changed = true
		}
	}
	if !changed {
		return payload, nil
	}
	out, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("%w: re-encode payload with inline bytes: %v", ErrPayloadInvalid, err)
	}
	return out, nil
}

func isBlobRef(url string) bool {
	return len(url) > len(blobURLPrefix) && url[:len(blobURLPrefix)] == blobURLPrefix
}

// writeBlob 以内容寻址落盘字节，返回 hex sha256。O_CREATE|O_EXCL 保证
// 并发/重复写入时同内容只落一份（EEXIST 视为成功——字节相同）。
func writeBlob(blobsDir string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	path := filepath.Join(blobsDir, sha)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return sha, nil // 同内容已存在：内容寻址天然去重
		}
		return "", fmt.Errorf("session: write blob %s: %w", sha, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", fmt.Errorf("session: write blob %s: %w", sha, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("session: write blob %s: %w", sha, err)
	}
	return sha, nil
}

func readBlob(blobsDir, sha string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(blobsDir, sha))
	if err != nil {
		return nil, fmt.Errorf("%w: blob %s missing or unreadable", ErrPayloadInvalid, sha)
	}
	// 内容寻址自校验：sha 与字节不符说明文件被外部篡改。
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != sha {
		return nil, fmt.Errorf("%w: blob %s checksum mismatch", ErrPayloadInvalid, sha)
	}
	return data, nil
}
