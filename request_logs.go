package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	requestLogMaxEntries   = 5000
	requestLogMaxAge       = 30 * 24 * time.Hour
	requestLogDefaultLimit = 50
	requestLogMaxLimit     = 100
)

type RequestLog struct {
	ID             string    `json:"id"`
	StartedAt      time.Time `json:"startedAt"`
	FinishedAt     time.Time `json:"finishedAt"`
	AccountID      string    `json:"accountId"`
	AccountEmail   string    `json:"accountEmail"`
	Protocol       string    `json:"protocol"`
	// Upstream 标记上游来源："cline"=Cline 账号池，"opencode"=opencode zen 免费模型
	Upstream     string `json:"upstream,omitempty"`
	Model        string `json:"model"`
	Stream       bool   `json:"stream"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	CachedTokens int64  `json:"cachedTokens"`
	TotalTokens  int64  `json:"totalTokens"`
	UsageAvailable bool `json:"usageAvailable"`
	DurationMs   int64     `json:"durationMs"`
	TTFTMs       int64     `json:"ttftMs"`
	OutputTPS    float64   `json:"outputTokensPerSecond"`
	Completed    bool      `json:"completed"`
	Error        string    `json:"error,omitempty"`
}

var (
	requestLogs     []RequestLog
	requestLogsMu   sync.Mutex
	requestLogsPath string
)

func init() {
	requestLogsPath = resolveDataPath(".cline-request-logs.json")
}

func loadRequestLogs() {
	data, err := os.ReadFile(requestLogsPath)
	if err != nil {
		return
	}
	var entries []RequestLog
	if err := json.Unmarshal(data, &entries); err != nil {
		// 坏文件隔离，避免下次保存覆盖销毁原始日志
		quarantineFile(requestLogsPath, err)
		return
	}
	requestLogsMu.Lock()
	requestLogs = pruneRequestLogsLocked(entries)
	requestLogsMu.Unlock()
}

// requestLogBefore 报告 a 是否应排在 b 之前（时间降序，平局 ID 降序）——
// pruneRequestLogsLocked 的统一比较器。
func requestLogBefore(a, b RequestLog) bool {
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.After(b.StartedAt)
	}
	return a.ID > b.ID
}

// isRequestLogsSorted O(n) 判定 entries 是否已按统一比较器降序（P5-10）：
// 有序时 prune 跳过 O(n log n) 全排（历史文件加载路径常态有序；热路径的
// 「降序头+追加尾」在连接处断序，自然回退全排）。
func isRequestLogsSorted(entries []RequestLog) bool {
	for i := 1; i < len(entries); i++ {
		if requestLogBefore(entries[i], entries[i-1]) {
			return false
		}
	}
	return true
}

func pruneRequestLogsLocked(entries []RequestLog) []RequestLog {
	if len(entries) == 0 {
		return entries
	}
	// P5-10：已按统一比较器降序时整段跳过 sort（输出顺序与全排恒等）
	if !isRequestLogsSorted(entries) {
		sort.Slice(entries, func(i, j int) bool {
			return requestLogBefore(entries[i], entries[j])
		})
	}

	cutoff := time.Now().Add(-requestLogMaxAge)
	pruned := entries[:0]
	for _, e := range entries {
		if e.StartedAt.Before(cutoff) {
			continue
		}
		pruned = append(pruned, e)
	}
	if len(pruned) > requestLogMaxEntries {
		pruned = pruned[:requestLogMaxEntries]
	}
	return pruned
}

func saveRequestLogsLocked() {
	// P5-10：紧凑 Marshal——文件体积约降 40%，防抖落盘序列化成本同步下降；
	// 消费方（loadRequestLogs / 管理端列表读内存）均无缩进格式依赖
	data, err := json.Marshal(requestLogs)
	if err != nil {
		return
	}
	if err := writeFileAtomic(requestLogsPath, data, 0600); err != nil {
		// 写失败保留脏标记，交给 flushRequestLogsDirty 下轮重试
		return
	}
	requestLogsDirty = false
}

// requestLogsDirty 与防抖器支撑逐请求日志的合并落盘（P3-1/P3-14：此前每请求
// 全量 sort + 重写 ≤5000 条）。只能在持有 requestLogsMu 时读写。
var (
	requestLogsDirty     bool
	requestLogsDebouncer = newDebouncedFlush(time.Second, flushRequestLogsDirty)
)

// flushRequestLogsDirty 由 requestLogsDebouncer 触发：锁内 prune（排序+裁剪）+
// 落盘，成功清脏。
func flushRequestLogsDirty() {
	requestLogsMu.Lock()
	defer requestLogsMu.Unlock()
	if !requestLogsDirty {
		return
	}
	requestLogs = pruneRequestLogsLocked(requestLogs)
	saveRequestLogsLocked()
}

func appendRequestLog(entry RequestLog) {
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("req_%d", entry.StartedAt.UnixNano())
	}
	requestLogsMu.Lock()
	requestLogs = append(requestLogs, entry)
	requestLogsDirty = true
	requestLogsMu.Unlock()
	requestLogsDebouncer.schedule()
}

type requestLogPage struct {
	Items      []RequestLog `json:"items"`
	NextCursor string       `json:"nextCursor"`
	HasMore    bool         `json:"hasMore"`
}

func encodeCursor(entry RequestLog) string {
	key := fmt.Sprintf("%d|%s", entry.StartedAt.UnixNano(), entry.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	// P3-13：严格解析 ts|id（Sscanf 会静默吞掉尾部垃圾）
	tsStr, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid cursor")
	}
	return time.Unix(0, ts), id, nil
}

func listRequestLogs(limit int, cursor string) (requestLogPage, error) {
	if limit <= 0 {
		limit = requestLogDefaultLimit
	}
	if limit > requestLogMaxLimit {
		limit = requestLogMaxLimit
	}

	var afterTime time.Time
	var afterID string
	if cursor != "" {
		t, id, err := decodeCursor(cursor)
		if err != nil {
			return requestLogPage{}, err
		}
		afterTime = t
		afterID = id
	}

	requestLogsMu.Lock()
	defer requestLogsMu.Unlock()

	// 防抖落盘期间内存切片未排序/未裁剪；分页依赖降序，读取侧补一次 prune
	// （幂等、不写盘不清脏，落盘由 flushRequestLogsDirty 收口）。
	if requestLogsDirty {
		requestLogs = pruneRequestLogsLocked(requestLogs)
	}

	result := make([]RequestLog, 0, limit)
	var lastEntry RequestLog
	for _, e := range requestLogs {
		if cursor != "" {
			if e.StartedAt.After(afterTime) {
				continue
			}
			if e.StartedAt.Equal(afterTime) && e.ID >= afterID {
				continue
			}
		}
		result = append(result, e)
		lastEntry = e
		if len(result) >= limit {
			break
		}
	}

	page := requestLogPage{Items: result}
	if len(result) == limit {
		page.NextCursor = encodeCursor(lastEntry)
		page.HasMore = true
	}
	return page, nil
}

func finalizeRequestLog(entry *RequestLog, usage tokenUsage, firstOutputAt time.Time, startedAt time.Time, completed bool, errMsg string) {
	entry.FinishedAt = time.Now()
	entry.DurationMs = entry.FinishedAt.Sub(startedAt).Milliseconds()
	entry.Completed = completed
	entry.Error = truncate(errMsg, 200)

	if usage.Valid {
		entry.UsageAvailable = true
		entry.InputTokens = usage.Prompt
		entry.OutputTokens = usage.Completion
		entry.CachedTokens = usage.Cached
		entry.TotalTokens = usage.Total
	}

	if !firstOutputAt.IsZero() && usage.Valid && usage.Completion > 0 {
		entry.TTFTMs = firstOutputAt.Sub(startedAt).Milliseconds()
		generationMs := entry.FinishedAt.Sub(firstOutputAt).Seconds()
		if generationMs > 0 {
			entry.OutputTPS = float64(usage.Completion) / generationMs
		}
	}

	appendRequestLog(*entry)
}

// markClineAttempt 标记 cline 上游并回填最后尝试的账号——成功与失败路径共用；
// 此前错误分支在回填前就落日志，管理端失败行的账号列显示为空（v1.3.5）。
func markClineAttempt(reqLog *RequestLog, acc *Account) {
	reqLog.Upstream = upstreamCline
	if acc != nil {
		reqLog.AccountID = acc.AccountID
		reqLog.AccountEmail = acc.Email
	}
}
