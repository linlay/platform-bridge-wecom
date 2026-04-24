// Package crypto 实现企业微信入站媒体的解密（AES-CBC + PKCS#7）。
//
// 严格对齐 Java WecomIncomingFileService.maybeDecrypt：
//   - aesKey：base64 字符串（允许缺 padding），decode 后必须是 32 字节
//   - iv = key[0:16]
//   - AES/CBC/NoPadding
//   - 密文若非 16 字节对齐，先 zero-pad 到对齐再解密（Java 防御行为）
//   - 解密后去 PKCS#7 padding；padLen 范围 1..32
//   - 异常情况：aesKey 空 → 返回原 bytes；aesKey 非 32B → 返回原 bytes（Java fallback）
package wecom

import (
	"crypto/aes"
	ccipher "crypto/cipher"
	"encoding/base64"
	"fmt"
	"strings"
)

// Decrypt 解密入站媒体 blob。
func Decrypt(encrypted []byte, aesKeyB64 string) ([]byte, error) {
	if len(encrypted) == 0 || strings.TrimSpace(aesKeyB64) == "" {
		return encrypted, nil
	}
	key, ok := decodeBase64ForgivePadding(aesKeyB64)
	if !ok || len(key) != 32 {
		// 对齐 Java：非 32B key 不报错，原样透传
		return encrypted, nil
	}
	aligned := alignTo16(encrypted)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes.NewCipher: %w", err)
	}
	iv := make([]byte, 16)
	copy(iv, key[:16])
	out := make([]byte, len(aligned))
	ccipher.NewCBCDecrypter(block, iv).CryptBlocks(out, aligned)
	return stripPKCS7(out)
}

func alignTo16(in []byte) []byte {
	rem := len(in) % 16
	if rem == 0 {
		return in
	}
	padded := make([]byte, len(in)+(16-rem))
	copy(padded, in)
	return padded
}

func stripPKCS7(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("crypto: decrypted payload is empty")
	}
	padLen := int(b[len(b)-1])
	if padLen < 1 || padLen > 32 || padLen > len(b) {
		return nil, fmt.Errorf("crypto: invalid pkcs7 padding value: %d", padLen)
	}
	for i := len(b) - padLen; i < len(b); i++ {
		if int(b[i]) != padLen {
			return nil, fmt.Errorf("crypto: invalid pkcs7 padding bytes at %d", i)
		}
	}
	return b[:len(b)-padLen], nil
}

// decodeBase64ForgivePadding 容忍 base64 缺 `=` padding。
func decodeBase64ForgivePadding(s string) ([]byte, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	if b, err := base64.StdEncoding.DecodeString(padBase64(s)); err == nil {
		return b, true
	}
	if b, err := base64.URLEncoding.DecodeString(padBase64(s)); err == nil {
		return b, true
	}
	return nil, false
}

func padBase64(s string) string {
	if n := len(s) % 4; n != 0 {
		return s + strings.Repeat("=", 4-n)
	}
	return s
}
