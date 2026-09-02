package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// clineStreamAggMaxBytes 限制聚合时整段响应的读取上限，防异常上游流撑爆内存。
const clineStreamAggMaxBytes = 32 << 20

// upstreamStreamForNonStream 报告非流式客户端请求是否改发上游流式请求再在代理侧
// 聚合（v1.3.6）。配置字段为 nil（旧配置文件无此键）时默认开启——存量部署零操作
// 获得修复；管理端显式关闭则回退旧行为。
func upstreamStreamForNonStream(cfg *proxyConfigData) bool {
	return cfg == nil || cfg.ForceUpstreamStream == nil || *cfg.ForceUpstreamStream
}

// aggregateClineStreamToChat 把上游 SSE 流聚合成标准非流式 chat.completion JSON 响应。
//
// 背景：api.cline.bot 网关的非流式 JSON 装配路径远比流式透传脆（实测同时间窗
// 非流式 0/5 vs 流式 5/5，v1.3.4 的 5xx 重试只是概率性缓解），非流式客户端请求
// 默认改发 stream:true 后由本函数聚合回 JSON（v1.3.6）。
//
// 兼容：上游若无视 stream:true 仍回非流式 JSON（首个非空行不以 "data:" 开头），
// 原样透传，不做聚合。流中出现 error 事件时返回 clineAPIError{502}（状态 ≥500
// 会让 callClineAPI 的账号 walk 自动接力下一账号）。
func aggregateClineStreamToChat(resp *http.Response, fallbackModel string) (*http.Response, error) {
	body, err := readAllBounded(resp.Body, clineStreamAggMaxBytes)
	resp.Body.Close()
	if err != nil {
		return nil, &clineAPIError{statusCode: http.StatusBadGateway, message: fmt.Sprintf("read upstream stream: %v", err)}
	}

	if !looksLikeSSE(body) {
		// 上游回了普通 JSON：原样透传（合成响应只重包已读取的字节）
		return synthJSONResponse(resp, body), nil
	}

	agg := newClineStreamAgg(fallbackModel)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue // 空行 / "event:" / 注释 / [DONE] 前置行
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue // 坏块跳过，与流式透传语义一致
		}
		obj = unwrapDataEnvelope(obj)
		if errPayload, ok := obj["error"]; ok && errPayload != nil {
			errBody, _ := json.Marshal(errPayload)
			return nil, &clineAPIError{statusCode: http.StatusBadGateway, message: truncate(string(errBody), 500)}
		}
		agg.consume(obj)
	}
	if !agg.sawChunk {
		return nil, &clineAPIError{statusCode: http.StatusBadGateway, message: "empty upstream stream"}
	}

	out, err := json.Marshal(agg.build())
	if err != nil {
		return nil, &clineAPIError{statusCode: http.StatusBadGateway, message: "marshal aggregated response: " + err.Error()}
	}
	return synthJSONResponse(resp, out), nil
}

// looksLikeSSE 判定 body 是否为 SSE 流：首个非空行以 "data:" 开头。
// JSON 响应体可能在内嵌字符串里出现 "data:"，只看首行可避免误判。
func looksLikeSSE(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		return strings.HasPrefix(trimmed, "data:")
	}
	return false
}

// readAllBounded 读取全部内容，超过 limit 视为异常而非静默截断（截断会产出坏 JSON）。
func readAllBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

// synthJSONResponse 用已读取的字节合成非流式 JSON 响应（原 resp.Body 已消费不可复用）。
func synthJSONResponse(orig *http.Response, body []byte) *http.Response {
	h := http.Header{}
	for k, vs := range orig.Header {
		switch {
		case strings.EqualFold(k, "Content-Length"):
			continue // 长度按新 body 重算
		case strings.EqualFold(k, "Content-Type"):
			continue // 聚合分支原头是 text/event-stream，统一改写（见下）
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    orig.StatusCode,
		Status:        orig.Status,
		Proto:         orig.Proto,
		ProtoMajor:    orig.ProtoMajor,
		ProtoMinor:    orig.ProtoMinor,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       orig.Request,
	}
}

// clineStreamAgg 聚合 SSE chunk 的累积状态。
type clineStreamAgg struct {
	fallbackModel string
	id            string
	model         string
	created       int64
	content       strings.Builder
	reasoning     strings.Builder
	reasoningAlt  strings.Builder // delta["reasoning_content"]（deepseek 生态键名）
	finish        string
	usage         map[string]any
	toolCalls     map[int]*clineStreamToolCall
	toolOrder     []int
	sawChunk      bool
}

// clineStreamToolCall 按 index 合并的 tool_call 分片（首片段定 id/name，arguments 追加）。
type clineStreamToolCall struct {
	id   string
	name string
	args strings.Builder
}

func newClineStreamAgg(fallbackModel string) *clineStreamAgg {
	return &clineStreamAgg{fallbackModel: fallbackModel, toolCalls: map[int]*clineStreamToolCall{}}
}

func (a *clineStreamAgg) consume(obj map[string]any) {
	a.sawChunk = true
	if a.id == "" {
		if id, ok := obj["id"].(string); ok {
			a.id = id
		}
	}
	if m, ok := obj["model"].(string); ok && m != "" && a.model == "" {
		a.model = m
	}
	if a.created == 0 {
		if c, ok := obj["created"].(float64); ok {
			a.created = int64(c)
		}
	}
	if u, ok := obj["usage"].(map[string]any); ok && u != nil {
		a.usage = u // 末块携带完整 usage，最后写入者生效
	}
	choices, _ := obj["choices"].([]any)
	for _, c := range choices {
		ch, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			a.finish = fr
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if s, ok := delta["content"].(string); ok {
			a.content.WriteString(s)
		}
		if s, ok := delta["reasoning"].(string); ok {
			a.reasoning.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			a.reasoningAlt.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for j, t := range tcs {
				tc, ok := t.(map[string]any)
				if !ok {
					continue
				}
				tIdx := j
				if fi, ok := tc["index"].(float64); ok {
					tIdx = int(fi)
				}
				agg := a.toolCalls[tIdx]
				if agg == nil {
					agg = &clineStreamToolCall{}
					a.toolCalls[tIdx] = agg
					a.toolOrder = append(a.toolOrder, tIdx)
				}
				if id, ok := tc["id"].(string); ok && id != "" {
					agg.id = id
				}
				if fn, ok := tc["function"].(map[string]any); ok && fn != nil {
					if n, ok := fn["name"].(string); ok && n != "" {
						agg.name = n
					}
					if args, ok := fn["arguments"].(string); ok {
						agg.args.WriteString(args)
					}
				}
			}
		}
	}
}

// build 合成标准非流式 chat.completion 结构（choices[0] 承载聚合结果，多 choice
// 上游在此网关未观测到，按单 choice 输出）。
func (a *clineStreamAgg) build() map[string]any {
	msg := map[string]any{"role": "assistant"}
	if a.content.Len() > 0 {
		msg["content"] = a.content.String()
	}
	if a.reasoningAlt.Len() > 0 {
		msg["reasoning_content"] = a.reasoningAlt.String()
	} else if a.reasoning.Len() > 0 {
		msg["reasoning"] = a.reasoning.String()
	}
	if len(a.toolCalls) > 0 {
		calls := make([]any, 0, len(a.toolOrder))
		for _, idx := range a.toolOrder {
			tc := a.toolCalls[idx]
			calls = append(calls, map[string]any{
				"id":   tc.id,
				"type": "function",
				"function": map[string]any{
					"name":      tc.name,
					"arguments": tc.args.String(),
				},
			})
		}
		msg["tool_calls"] = calls
	}
	var finish any
	if a.finish != "" {
		finish = a.finish
	}
	id := a.id
	if id == "" {
		id = fmt.Sprintf("chatcmpl-agg-%d", time.Now().UnixNano())
	}
	model := a.model
	if model == "" {
		model = a.fallbackModel
	}
	created := a.created
	if created == 0 {
		created = time.Now().Unix()
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if a.usage != nil {
		out["usage"] = a.usage
	}
	return out
}
