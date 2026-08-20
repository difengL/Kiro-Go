package proxy

// TEMP DEBUG — 排查"模型表达意图但不调工具"复现，确认后删除。
// 只记录两个关键点：请求侧结构 + 事件流是否有 toolUse。

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kiro-go/config"
)

func dbgLog(format string, v ...interface{}) {
	dir := config.GetConfigDir()
	if dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "evdbg.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s "+format+"\n", append([]interface{}{time.Now().Format("15:04:05.000")}, v...)...)
}
