package protocol

import (
	"strings"
	"testing"
)

// Issue 生成的 ticket 必须字节对齐 Java DownstreamAgentTicketService.generateJwtTicket：
// - 三段，第三段空（保留尾部点）
// - base64url 无 padding
// - header 字段固定顺序 alg/ak/ch
// - ak/ch 小写归一，sub trim 但不 lowercase
func TestIssueExactWire(t *testing.T) {
	tk, err := IssueTicket("Wecom:XiaoZhai", "user-42", "  MyAgent  ")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	parts := strings.Split(tk, ".")
	if len(parts) != 3 || parts[2] != "" {
		t.Fatalf("want 3 parts with empty third; got %q", tk)
	}
	hdr, err := b64urlDecode(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	want := `{"alg":"none","ak":"myagent","ch":"wecom:xiaozhai"}`
	if string(hdr) != want {
		t.Fatalf("header mismatch:\n got  %s\n want %s", hdr, want)
	}
	pl, err := b64urlDecode(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if string(pl) != `{"sub":"user-42"}` {
		t.Fatalf("payload mismatch: %s", pl)
	}
	// 无 padding
	if strings.ContainsRune(parts[0]+parts[1], '=') {
		t.Fatalf("base64url should have no padding")
	}
}

// ValidateTicket: 四元严格匹配 claims（normalized）
func TestValidateTicketFourArgs(t *testing.T) {
	tk, _ := IssueTicket("wecom:xiaozhai", "u1", "agentA")
	c, ok := ValidateTicket("WeCom:XiaoZhai", "u1", "AGENTA", tk) // case mismatch in args, normalize 后应等
	if !ok {
		t.Fatalf("expected ok with normalized match")
	}
	if c.UserID != "u1" || c.AgentKey != "agenta" || c.Channel != "wecom:xiaozhai" {
		t.Fatalf("claims: %+v", c)
	}

	if _, ok := ValidateTicket("wecom:xiaozhai", "u2", "agentA", tk); ok {
		t.Fatalf("userId mismatch should fail")
	}
	if _, ok := ValidateTicket("wecom:other", "u1", "agentA", tk); ok {
		t.Fatalf("channel mismatch should fail")
	}
	if _, ok := ValidateTicket("wecom:xiaozhai", "u1", "other", tk); ok {
		t.Fatalf("agentKey mismatch should fail")
	}
	if _, ok := ValidateTicket("", "u1", "agentA", tk); ok {
		t.Fatalf("blank channel should fail")
	}
}

// ValidateToken: 三元（channel+agentKey+token）；从 claims 取 userId
func TestValidateToken(t *testing.T) {
	tk, _ := IssueTicket("wecom:xiaozhai", "u1", "agentA")
	c, ok := ValidateToken("wecom:xiaozhai", "agentA", tk)
	if !ok || c.UserID != "u1" {
		t.Fatalf("ValidateToken: ok=%v claims=%+v", ok, c)
	}
	if _, ok := ValidateToken("wecom:other", "agentA", tk); ok {
		t.Fatalf("channel mismatch should fail")
	}
}

// ValidateAny: 单 token，全部从 claims 提取
func TestValidateAny(t *testing.T) {
	tk, _ := IssueTicket("wecom:xiaozhai", "u1", "agentA")
	c, ok := ValidateAny(tk)
	if !ok {
		t.Fatalf("ValidateAny should succeed")
	}
	if c.UserID != "u1" || c.AgentKey != "agenta" || c.Channel != "wecom:xiaozhai" {
		t.Fatalf("claims: %+v", c)
	}
	// tampered alg → reject
	if _, ok := ValidateAny(mutateHeaderAlg(t, tk, "HS256")); ok {
		t.Fatalf("non-'none' alg must be rejected")
	}
	// not 3 parts
	if _, ok := ValidateAny("aaa.bbb"); ok {
		t.Fatalf("2-part must be rejected")
	}
	// blank sub → reject
	if _, ok := ValidateAny(issueWith(t, map[string]string{"alg": "none", "ak": "x", "ch": "y"}, map[string]string{"sub": ""})); ok {
		t.Fatalf("blank sub must be rejected")
	}
}

// 帮助函数：替换 header 中 alg 字段，保留其余结构
func mutateHeaderAlg(t *testing.T, tk, alg string) string {
	t.Helper()
	parts := strings.Split(tk, ".")
	hdr, _ := b64urlDecode(parts[0])
	// 简单替换 "alg":"none" → "alg":"<alg>"
	h2 := strings.Replace(string(hdr), `"alg":"none"`, `"alg":"`+alg+`"`, 1)
	return b64urlEncode([]byte(h2)) + "." + parts[1] + "."
}

func issueWith(t *testing.T, header, payload map[string]string) string {
	t.Helper()
	h := `{"alg":"` + header["alg"] + `","ak":"` + header["ak"] + `","ch":"` + header["ch"] + `"}`
	p := `{"sub":"` + payload["sub"] + `"}`
	return b64urlEncode([]byte(h)) + "." + b64urlEncode([]byte(p)) + "."
}
