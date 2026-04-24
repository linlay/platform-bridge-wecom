// Package media 处理企业微信媒体下载（+解密）和上传（分块）。
package wecom

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

)

type Fetched struct {
	Bytes       []byte
	SHA256      string
	ContentType string
	Filename    string
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Fetch 下载企微给的加密 blob，按 aesKey 解密后返回。
// aesKey 为空 → 原样返回。
func Fetch(url, aesKey string) (Fetched, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return Fetched{}, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Fetched{}, fmt.Errorf("media fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Fetched{}, fmt.Errorf("media fetch: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Fetched{}, err
	}
	plain, err := Decrypt(raw, aesKey)
	if err != nil {
		return Fetched{}, fmt.Errorf("media decrypt: %w", err)
	}
	sum := sha256.Sum256(plain)
	return Fetched{
		Bytes:       plain,
		SHA256:      hex.EncodeToString(sum[:]),
		ContentType: strings.TrimSpace(resp.Header.Get("Content-Type")),
		Filename:    extractFilename(resp.Header.Get("Content-Disposition")),
	}, nil
}

func extractFilename(cd string) string {
	if cd == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(cd); err == nil {
		if v, ok := params["filename*"]; ok {
			return decodeRFC5987(v)
		}
		if v, ok := params["filename"]; ok {
			return v
		}
	}
	return ""
}

// decodeRFC5987 处理 filename*=UTF-8''xxx 这种编码。
func decodeRFC5987(v string) string {
	// 形如 "UTF-8''foo%20bar"
	parts := strings.SplitN(v, "''", 2)
	if len(parts) != 2 {
		return v
	}
	decoded, err := pctDecode(parts[1])
	if err != nil {
		return parts[1]
	}
	return decoded
}

func pctDecode(s string) (string, error) {
	return (&pctDecoder{}).decode(s)
}

type pctDecoder struct{}

func (*pctDecoder) decode(s string) (string, error) {
	// 避免引入 net/url 的 PathUnescape 把 + 当空格
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("bad pct")
		}
		hi := fromHex(s[i+1])
		lo := fromHex(s[i+2])
		if hi < 0 || lo < 0 {
			return "", fmt.Errorf("bad pct hex")
		}
		b.WriteByte(byte(hi<<4 | lo))
		i += 2
	}
	return b.String(), nil
}

func fromHex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c - 'a' + 10)
	case c >= 'A' && c <= 'F':
		return int(c - 'A' + 10)
	}
	return -1
}
