// Package diag 提供极简结构化日志。
//
// 输出格式: "<level> <event> k1=v1 k2=v2 ..."（一行一条，便于 grep）。
// 不引入任何第三方日志库，保持 bridge 二进制体积和启动速度。
package diag

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
)

type level int32

const (
	levelDebug level = iota
	levelInfo
	levelWarn
	levelError
)

var current atomic.Int32 // 默认 levelInfo = 1 (int32 零值为 0 = debug，Configure 修正)

func Configure(name string) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		current.Store(int32(levelDebug))
	case "warn":
		current.Store(int32(levelWarn))
	case "error":
		current.Store(int32(levelError))
	default:
		current.Store(int32(levelInfo))
	}
}

func Debug(event string, kv ...any) { emit(levelDebug, "DEBUG", event, kv) }
func Info(event string, kv ...any)  { emit(levelInfo, "INFO", event, kv) }
func Warn(event string, kv ...any)  { emit(levelWarn, "WARN", event, kv) }
func Error(event string, kv ...any) { emit(levelError, "ERROR", event, kv) }

func emit(l level, tag, event string, kv []any) {
	if int32(l) < current.Load() {
		return
	}
	var b strings.Builder
	b.WriteString(tag)
	b.WriteByte(' ')
	b.WriteString(event)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteByte(' ')
		fmt.Fprintf(&b, "%v=%v", kv[i], kv[i+1])
	}
	log.Println(b.String())
}
