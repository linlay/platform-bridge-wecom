// Package chatid 定义 bridge 使用的 WeCom chatId 格式。
//
// 格式：wecom#{chatType}#{sourceId}#{base36(seq)}
//   - chatType ∈ {single, group}
//   - seq = epochSeconds*1000 + secondSequence，base36 小写，保证单调递增
//   - sourceId 允许内部 `#`；解析时最后一段 base36 作为 seq，前面全部作为 sourceId
//
// 对应 SPEC.md §3。
package protocol

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Parsed struct {
	ChatType string // single | group
	SourceID string
	Seq      string // base36 lowercase
}

var reChatID = regexp.MustCompile(`^wecom#(group|single)#(.+)#([0-9a-z]+)$`)

// Parse 按 regex 反解 chatId；最后一段小写 base36 做 seq。
func Parse(s string) (Parsed, error) {
	m := reChatID.FindStringSubmatch(s)
	if m == nil {
		return Parsed{}, fmt.Errorf("chatid: invalid format %q", s)
	}
	if m[2] == "" {
		return Parsed{}, fmt.Errorf("chatid: empty sourceId in %q", s)
	}
	return Parsed{ChatType: m[1], SourceID: m[2], Seq: m[3]}, nil
}

// Formatter 负责生成单调递增的 seq。并发安全。
type Formatter struct {
	mu      sync.Mutex
	lastSec int64
	inSec   int64 // 本秒的 counter 0..999；超过 999 则进位到下一秒
}

func NewFormatter() *Formatter { return &Formatter{} }

// Format 生成 chatId；传入 now 以便测试可控时间。
func (f *Formatter) Format(chatType, sourceID string, now time.Time) string {
	sec := now.Unix()
	f.mu.Lock()
	if sec <= f.lastSec {
		f.inSec++
		if f.inSec > 999 {
			f.lastSec++
			f.inSec = 0
		}
		sec = f.lastSec
	} else {
		f.lastSec = sec
		f.inSec = 0
	}
	seq := sec*1000 + f.inSec
	f.mu.Unlock()
	return "wecom#" + chatType + "#" + sourceID + "#" + strings.ToLower(strconv.FormatInt(seq, 36))
}
