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

// Formatter 负责生成单调递增的 seq、并按 (appKey, chatType, sourceId) 缓存 chatId。
// 对齐 Java WecomSessionChatIdService.resolveOrCreate —— 同一用户在同一 bot 里
// 跨消息复用同一 chatId，agent 沙箱挂载的 /workspace 才能在多轮对话里看到之前
// 上传的文件。缓存仅在内存，重启清空（与 Java 的 ConcurrentMap 同语义）。
type Formatter struct {
	mu      sync.Mutex
	lastSec int64
	inSec   int64 // 本秒的 counter 0..999；超过 999 则进位到下一秒

	// sourceKey("<appKey>|<chatType>|<sourceId>") → chatId
	cache map[string]string
	// chatId → appKey：多 bot 场景下 platform 出站消息（chat.updated）到达时
	// bridge 靠这张表反查"该 chatId 归哪个 bot"，路由到正确的 wecom.Client。
	// 与 cache 同步写入，命中率取决于 bridge 进程生命周期（重启会丢失）。
	ownerByChatID map[string]string
}

func NewFormatter() *Formatter {
	return &Formatter{
		cache:         map[string]string{},
		ownerByChatID: map[string]string{},
	}
}

// Format 始终生成新 chatId（保留给特殊场景；日常入站消息走 ResolveOrCreate）。
// 传入 now 以便测试可控时间。
func (f *Formatter) Format(chatType, sourceID string, now time.Time) string {
	f.mu.Lock()
	seq := f.nextSeqLocked(now)
	f.mu.Unlock()
	return "wecom#" + chatType + "#" + sourceID + "#" + strings.ToLower(strconv.FormatInt(seq, 36))
}

// ResolveOrCreate 对齐 Java WecomSessionChatIdService.resolveOrCreate：
// 按 (appKey, chatType, sourceId) 缓存 chatId；首次访问才生成新 seq，后续复用。
// appKey 允许为空（单租户 bridge 场景），空串归一化为 "default"。
func (f *Formatter) ResolveOrCreate(appKey, chatType, sourceID string, now time.Time) string {
	if appKey == "" {
		appKey = "default"
	}
	key := appKey + "|" + chatType + "|" + sourceID
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.cache[key]; ok {
		return existing
	}
	seq := f.nextSeqLocked(now)
	id := "wecom#" + chatType + "#" + sourceID + "#" + strings.ToLower(strconv.FormatInt(seq, 36))
	f.cache[key] = id
	f.ownerByChatID[id] = appKey
	return id
}

// OwnerAppKey 返回 chatId 归属的 bot appKey。未见过的 chatId 返回 ok=false；
// bridge 重启后 cache 重建前命中率为零，需要调用方做兜底（通常是按默认 bot 或报错）。
func (f *Formatter) OwnerAppKey(chatID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	appKey, ok := f.ownerByChatID[chatID]
	return appKey, ok
}

// RegisterOwner 手动绑定 chatId → appKey，给 bridge 重启后从持久化状态恢复用。
// Phase B 暂未持久化，保留钩子。
func (f *Formatter) RegisterOwner(chatID, appKey string) {
	if chatID == "" || appKey == "" {
		return
	}
	f.mu.Lock()
	f.ownerByChatID[chatID] = appKey
	f.mu.Unlock()
}

// nextSeqLocked 必须在持有 f.mu 状态下调用。
func (f *Formatter) nextSeqLocked(now time.Time) int64 {
	sec := now.Unix()
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
	return sec*1000 + f.inSec
}
