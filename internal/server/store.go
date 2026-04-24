// Package store 提供 bridge 的本地 blob 存储，用作平台上传/下载的文件仓。
//
// Phase 2 只做文件系统持久化，路径结构：<root>/objects/<userId>/<chatId>/<fileId>
// 元数据伴生文件：<fileId>.meta.json
//
// SPEC.md §5 要求 /api/push 的 upload.sha256 必填，store 在 Put 时计算并回填。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Meta struct {
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type FileStore struct {
	root string
}

func NewFileStore(root string) (*FileStore, error) {
	if err := os.MkdirAll(filepath.Join(root, "objects"), 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir root: %w", err)
	}
	return &FileStore{root: root}, nil
}

// Put 写入对象并计算 SHA256 + size。如果 meta.SHA256/SizeBytes 已有值，会被覆盖。
func (s *FileStore) Put(userID, chatID, fileID string, r io.Reader, meta Meta) (Meta, error) {
	dir := s.objDir(userID, chatID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Meta{}, err
	}
	dataPath := filepath.Join(dir, fileID)
	f, err := os.Create(dataPath)
	if err != nil {
		return Meta{}, err
	}
	defer f.Close()

	h := sha256.New()
	mw := io.MultiWriter(f, h)
	n, err := io.Copy(mw, r)
	if err != nil {
		return Meta{}, err
	}
	meta.SHA256 = hex.EncodeToString(h.Sum(nil))
	meta.SizeBytes = n

	mb, err := json.Marshal(meta)
	if err != nil {
		return Meta{}, err
	}
	if err := os.WriteFile(dataPath+".meta.json", mb, 0o644); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// Get 返回对象字节流 + meta。调用方负责 Close rc。
func (s *FileStore) Get(userID, chatID, fileID string) (io.ReadCloser, Meta, error) {
	dataPath := filepath.Join(s.objDir(userID, chatID), fileID)
	mb, err := os.ReadFile(dataPath + ".meta.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Meta{}, fmt.Errorf("store: object not found: %s", dataPath)
		}
		return nil, Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(mb, &meta); err != nil {
		return nil, Meta{}, err
	}
	f, err := os.Open(dataPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Meta{}, fmt.Errorf("store: object not found: %s", dataPath)
		}
		return nil, Meta{}, err
	}
	return f, meta, nil
}

func (s *FileStore) objDir(userID, chatID string) string {
	// chatID 含 `#`，在 Windows 下可作为文件名；为隔离做简单编码替换保险起见
	safe := strings.ReplaceAll(chatID, string(filepath.Separator), "_")
	return filepath.Join(s.root, "objects", userID, safe)
}
