// Package registry 持久化 chatId → 企业微信回复目标 的映射。
//
// 场景：入站用户消息时记录 (chatId → appKey + receiveId + receiveIdType)；
// chat.updated 推送来时反查目标以回传 markdown。
// Phase 2 的 store 只管文件 blob；这里是独立的 JSON 索引。
package wecom

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type Target struct {
	AppKey        string `json:"appKey"`
	ReceiveID     string `json:"receiveId"`
	ReceiveIDType string `json:"receiveIdType"` // "userid" or "chatid"

	// SourceReqID 是入站 wecom callback 的 headers.req_id，aibot_respond_msg stream
	// 模式必须回显这个 id（wecom 按它续写同一条消息气泡）。
	SourceReqID string `json:"sourceReqId,omitempty"`
}

type Registry struct {
	path string
	mu   sync.Mutex
	m    map[string]Target
}

func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, m: make(map[string]Target)}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, err
			}
			return r, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &r.m); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) Register(chatID string, t Target) {
	r.mu.Lock()
	r.m[chatID] = t
	b, _ := json.Marshal(r.m)
	r.mu.Unlock()
	tmp := r.path + ".tmp"
	_ = os.WriteFile(tmp, b, 0o644)
	_ = os.Rename(tmp, r.path)
}

func (r *Registry) Lookup(chatID string) (Target, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.m[chatID]
	return t, ok
}
