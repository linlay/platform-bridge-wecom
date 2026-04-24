// Package ticket 实现 aiagent-gateway 的无签名 JWT 票据协议。
//
// 严格对齐 Java DownstreamAgentTicketService：
//   - header 字段顺序 alg/ak/ch（LinkedHashMap）
//   - payload 字段 sub
//   - base64url 无 padding
//   - 三段用 "." 分隔，第三段空（保留尾部点）
//   - ak/ch 以 trim + lowercase 归一；sub 仅 trim
package protocol

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Claims 是解码并归一化后的票据声明。
type Claims struct {
	UserID   string
	AgentKey string
	Channel  string
}

// Issue 生成一个无签名 ticket。channel/agentKey 会 trim+lowercase，userId 只 trim。
func IssueTicket(channel, userID, agentKey string) (string, error) {
	ch := normalize(channel)
	ak := normalize(agentKey)
	sub := strings.TrimSpace(userID)
	if ch == "" || ak == "" || sub == "" {
		return "", fmt.Errorf("ticket: channel/userId/agentKey must be non-empty")
	}
	// 手搓 JSON 保证字段顺序（encoding/json 的 map 会按 key 排序，不符合 Java 的 LinkedHashMap 顺序）
	h, err := marshalHeader(ak, ch)
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(struct {
		Sub string `json:"sub"`
	}{Sub: sub})
	if err != nil {
		return "", err
	}
	return b64urlEncode(h) + "." + b64urlEncode(p) + ".", nil
}

// ValidateTicket 对应 Java validateTicket(channel, userId, agentKey, ticket)：
// 四个参数都非空，且解码后的 claims 在归一化意义上与入参一致。
func ValidateTicket(channel, userID, agentKey, token string) (Claims, bool) {
	if strings.TrimSpace(channel) == "" || strings.TrimSpace(userID) == "" ||
		strings.TrimSpace(agentKey) == "" || strings.TrimSpace(token) == "" {
		return Claims{}, false
	}
	c, ok := parse(token)
	if !ok {
		return Claims{}, false
	}
	if c.UserID != strings.TrimSpace(userID) ||
		c.AgentKey != normalize(agentKey) ||
		c.Channel != normalize(channel) {
		return Claims{}, false
	}
	return c, true
}

// ValidateToken 对应 Java validateToken(channel, agentKey, token)：
// 不校验 userId；从 claims 里取 userId 返回。
func ValidateToken(channel, agentKey, token string) (Claims, bool) {
	if strings.TrimSpace(channel) == "" || strings.TrimSpace(agentKey) == "" ||
		strings.TrimSpace(token) == "" {
		return Claims{}, false
	}
	c, ok := parse(token)
	if !ok {
		return Claims{}, false
	}
	if c.AgentKey != normalize(agentKey) || c.Channel != normalize(channel) {
		return Claims{}, false
	}
	return c, true
}

// ValidateAny 对应 Java validateTicket(ticket)：单参数，全从 claims 取。
func ValidateAny(token string) (Claims, bool) {
	if strings.TrimSpace(token) == "" {
		return Claims{}, false
	}
	return parse(token)
}

func parse(token string) (Claims, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return Claims{}, false
	}
	hb, err := b64urlDecode(parts[0])
	if err != nil {
		return Claims{}, false
	}
	var hdr struct {
		Alg string `json:"alg"`
		Ak  string `json:"ak"`
		Ch  string `json:"ch"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return Claims{}, false
	}
	if hdr.Alg != "none" {
		return Claims{}, false
	}
	pb, err := b64urlDecode(parts[1])
	if err != nil {
		return Claims{}, false
	}
	var pl struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(pb, &pl); err != nil {
		return Claims{}, false
	}
	c := Claims{
		UserID:   strings.TrimSpace(pl.Sub),
		AgentKey: normalize(hdr.Ak),
		Channel:  normalize(hdr.Ch),
	}
	if c.UserID == "" || c.AgentKey == "" || c.Channel == "" {
		return Claims{}, false
	}
	return c, true
}

func marshalHeader(ak, ch string) ([]byte, error) {
	// 顺序固定 alg/ak/ch，手工拼接避免 map 排序
	type hdr struct {
		Alg string `json:"alg"`
		Ak  string `json:"ak"`
		Ch  string `json:"ch"`
	}
	return json.Marshal(hdr{Alg: "none", Ak: ak, Ch: ch})
}

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func b64urlEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func b64urlDecode(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	return base64.RawURLEncoding.DecodeString(s)
}
