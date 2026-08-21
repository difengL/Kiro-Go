package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"kiro-go/logger"
)

// ==================== 上游 token 字段探针（临时排查用） ====================
//
// 存在的理由：updateTokensFromEvent 只认一份硬编码的字段名清单
// （inputTokens / cacheReadInputTokens / ...），collectUsageMaps 也只肯下钻
// 名为 usage / tokenUsage / token_usage 的键。所以"解析出来是 0"只能证明
// 「那些名字」不存在，不能证明上游没回传 token 数据 —— 换个字段名或换一层
// 嵌套结构，现有解析就会视而不见。
//
// 这个探针不带任何字段名先验：
//   1. 记录事件流里每一个数值型叶子字段的完整路径，无论键叫什么；
//   2. 对未被 parseEventStream 显式处理的事件类型，原样打印一次 payload，
//      连结构本身都不假设。
//
// 为避免把会话正文写进日志，承载文本的事件（assistantResponseEvent /
// reasoningContentEvent / toolUseEvent）不参与 payload 转储；字符串字段也
// 只在键名与 token 相关且值本身是纯数字时才记录。
//
// 排查结束后整个文件可以直接删掉，只需摘掉 parseEventStream 里的两处调用。

// tokenishKeyRe 仅用于筛选字符串型字段，数值字段一律照收。
var tokenishKeyRe = regexp.MustCompile(`(?i)token|cache|usage|billing|credit|prompt|completion|context`)

var numericStringRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// 这些事件类型携带会话正文，不转储 payload。
var contentBearingEvents = map[string]bool{
	"assistantResponseEvent": true,
	"reasoningContentEvent":  true,
	"toolUseEvent":           true,
}

const (
	probeMaxDumps      = 12
	probeMaxDumpLength = 600
)

type tokenFieldProbe struct {
	// numericFields 记录 路径 -> 最后一次见到的值，按路径去重，
	// 所以噪声上限是"不同路径的数量"而不是"事件数量"。
	numericFields map[string]string
	eventTypes    map[string]int
	dumpedTypes   map[string]bool
	dumpCount     int
}

func newTokenFieldProbe() *tokenFieldProbe {
	return &tokenFieldProbe{
		numericFields: make(map[string]string),
		eventTypes:    make(map[string]int),
		dumpedTypes:   make(map[string]bool),
	}
}

func (p *tokenFieldProbe) observe(eventType string, event map[string]interface{}, payload []byte) {
	if p == nil {
		return
	}

	label := eventType
	if label == "" {
		label = "<no-type-header>"
	}
	p.eventTypes[label]++

	p.walk(label, event)

	// 未显式处理的事件类型：原样打印一次，防止漏掉连键名都猜不到的结构。
	if contentBearingEvents[eventType] {
		return
	}
	switch eventType {
	case "meteringEvent", "contextUsageEvent":
		// 已知会处理的事件，结构清楚，不必转储。
		return
	}
	if p.dumpedTypes[label] || p.dumpCount >= probeMaxDumps {
		return
	}
	p.dumpedTypes[label] = true
	p.dumpCount++

	raw := string(payload)
	if len(raw) > probeMaxDumpLength {
		raw = raw[:probeMaxDumpLength] + "...(truncated)"
	}
	logger.Infof("[TokenProbe] 未处理事件类型 type=%s payload=%s", label, raw)
}

func (p *tokenFieldProbe) walk(prefix string, value interface{}) {
	switch node := value.(type) {
	case map[string]interface{}:
		for key, child := range node {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			switch leaf := child.(type) {
			case float64:
				p.numericFields[path] = strconv.FormatFloat(leaf, 'f', -1, 64)
			case json.Number:
				p.numericFields[path] = leaf.String()
			case bool:
				if tokenishKeyRe.MatchString(key) {
					p.numericFields[path] = strconv.FormatBool(leaf)
				}
			case string:
				// 字符串只在"键名相关 + 值是纯数字"时记录，避免把正文写进日志。
				if tokenishKeyRe.MatchString(key) && numericStringRe.MatchString(leaf) {
					p.numericFields[path] = leaf
				}
			default:
				p.walk(path, child)
			}
		}
	case []interface{}:
		for i, child := range node {
			p.walk(fmt.Sprintf("%s[%d]", prefix, i), child)
		}
	}
}

// probeTokenUsage 是探针自带的最小汇总结构。
//
// 探针刻意不复用 parseEventStream 的解析类型：它的价值就在于不带字段名先验，
// 依赖被解析侧的结构反而会让两者一起漂移。缓存相关字段已从解析侧移除
// （上游实测不回传），所以这里只留 input/output 两项与探针抓到的原始字段对照。
type probeTokenUsage struct {
	InputTokens  int
	OutputTokens int
}

// report 在流结束时打印汇总：见过哪些事件类型、抓到哪些数值字段，
// 以及现有解析逻辑最终得出的结果，三者对照就能判断是"上游没给"
// 还是"给了但我们没认出来"。
func (p *tokenFieldProbe) report(usage probeTokenUsage) {
	if p == nil {
		return
	}

	types := make([]string, 0, len(p.eventTypes))
	for name, count := range p.eventTypes {
		types = append(types, fmt.Sprintf("%s=%d", name, count))
	}
	sort.Strings(types)

	fields := make([]string, 0, len(p.numericFields))
	for path, val := range p.numericFields {
		fields = append(fields, path+"="+val)
	}
	sort.Strings(fields)

	fieldsDesc := strings.Join(fields, " ")
	if fieldsDesc == "" {
		fieldsDesc = "<无任何数值字段>"
	}

	logger.Infof("[TokenProbe] 事件类型: %s", strings.Join(types, " "))
	logger.Infof("[TokenProbe] 数值字段(%d): %s", len(p.numericFields), fieldsDesc)
	logger.Infof("[TokenProbe] 现有解析结果: input=%d output=%d",
		usage.InputTokens, usage.OutputTokens)
}
