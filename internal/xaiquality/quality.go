// Package xaiquality scores bounded, account-specific Grok 4.7 probes.
// Results describe observed answer quality, not the identity of a hidden model.
package xaiquality

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const Model = "grok-4.7"
const Version = "grok47-v1"

type Sample struct {
	Kind            string `json:"kind"`
	Prompt          string `json:"prompt"`
	Expected        int    `json:"expected"`
	Marker          string `json:"marker"`
	Answer          string `json:"answer,omitempty"`
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	Model           string `json:"model,omitempty"`
	HTTPStatus      int    `json:"http_status,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	OutputTokens    int    `json:"output_tokens,omitempty"`
	ReasoningTokens *int   `json:"reasoning_tokens,omitempty"`
}

type Removal struct {
	Status    string `json:"status"`
	PoolID    string `json:"pool_id"`
	PoolName  string `json:"pool_name"`
	PoolURL   string `json:"pool_url,omitempty"`
	FileName  string `json:"file_name"`
	AuthIndex string `json:"auth_index,omitempty"`
	At        int64  `json:"at"`
	Error     string `json:"error,omitempty"`
}

type Report struct {
	Version     string   `json:"version"`
	Model       string   `json:"model"`
	Status      string   `json:"status"`
	Blocked     bool     `json:"blocked"`
	Source      string   `json:"source"`
	At          int64    `json:"at"`
	Summary     string   `json:"summary"`
	Samples     []Sample `json:"samples,omitempty"`
	Removal     *Removal `json:"removal,omitempty"`
	ConfirmedAt int64    `json:"confirmed_at,omitempty"`
	RiskSamples []Sample `json:"risk_samples,omitempty"`
}

type Response struct {
	Status int
	Body   []byte
}
type Caller func(context.Context, []byte) (Response, error)

// Fresh challenges prevent a cached fixed answer from passing the next run.
func challenge(round int) Sample {
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		panic(err)
	}
	a, b, c := 7+int(seed[0])%13, 4+int(seed[1])%9, 3+int(seed[2])%7
	s := Sample{Marker: "cfm-" + hex.EncodeToString(seed[4:]), Status: "error"}
	switch round % 3 {
	case 0:
		s.Kind = "inventory"
		s.Prompt = fmt.Sprintf("仓库有%d箱，每箱%d袋，每袋%d个球。先取走%d袋，再取走%d个球，最后退回%d袋（每袋仍为原数量），剩余多少个球？", a, b, c, a-2, c, 2)
		// Removed/returned bags contain c balls; this is deliberately computed
		// separately from the prompt so the answer is never supplied to the model.
		s.Expected = a*b*c - (a-2)*c - c + 2*c
	case 1:
		s.Kind = "schedule"
		d, e := 2+int(seed[3])%9, 3+int(seed[4])%8
		left, right := a+c, b+d
		if right > left {
			left = right
		}
		s.Expected = left + e
		s.Prompt = fmt.Sprintf("任务A耗时%d分钟，B耗时%d分钟，均可在0分钟启动；C耗时%d分钟，必须等A结束；D耗时%d分钟，必须等B结束；E耗时%d分钟，必须等C和D都结束。资源无限，可并行，全部任务最早在第几分钟完成？", a, b, c, d, e)
	case 2:
		s.Kind = "counting"
		n := 170 + int(seed[0])
		for i := 1; i <= n; i++ {
			if (i%3 == 0 || i%5 == 0) && i%7 != 0 {
				s.Expected++
			}
		}
		s.Prompt = fmt.Sprintf("从1到%d（两端包含）的整数中，能被3或5整除、但不能被7整除的整数有多少个？同时被3和5整除只计一次。", n)
	}
	s.Prompt += fmt.Sprintf("只输出一个JSON对象，answer为整数答案，marker为字符串%q。不要解释，不要Markdown代码块。", s.Marker)
	return s
}

// Run always requires three complete answers. An anomaly triggers an independent
// three-question confirmation; errors, absent usage and speed alone never convict.
func Run(ctx context.Context, call Caller, previous *Report) Report {
	r := Report{Version: Version, Model: Model, Status: "normal", At: time.Now().Unix()}
	priorBlocked := previous != nil && previous.Blocked
	firstFailures, secondFailures, errors := 0, 0, 0
	for i := 0; i < 6; i++ {
		if i == 3 && firstFailures == 0 && !priorBlocked {
			break
		}
		s := challenge(i)
		payload, _ := json.Marshal(map[string]any{"model": Model, "input": []any{map[string]any{"role": "user", "content": s.Prompt}}, "stream": false, "max_output_tokens": 2048, "store": false})
		start := time.Now()
		response, err := call(ctx, payload)
		s.DurationMS = time.Since(start).Milliseconds()
		s.HTTPStatus = response.Status
		if err != nil {
			s.Reason = "请求失败或超时，未计为降智"
		} else {
			score(&s, response)
		}
		r.Samples = append(r.Samples, s)
		if s.Status == "error" {
			errors++
			break
		}
		if s.Status == "fail" {
			if i < 3 {
				firstFailures++
			} else {
				secondFailures++
			}
		}
	}
	switch {
	case errors > 0:
		r.Status = "error"
		r.Blocked = priorBlocked
		r.Summary = "检测未完成；请求、额度或响应格式异常，不能据此判断降智"
	case firstFailures >= 2 && secondFailures >= 2:
		r.Status = "high_risk"
		r.Blocked = true
		r.Summary = fmt.Sprintf("两组独立检测均至少2题失败（%d/6），标记降智风险并阻止交付", firstFailures+secondFailures)
	case firstFailures+secondFailures > 0:
		r.Status = "suspect"
		r.Blocked = true
		r.Summary = "发现答案或指令遵循异常，暂缓交付；复测全部通过后解除"
	default:
		r.Summary = "本轮可判分题目全部通过，未观察到降智信号"
	}
	if r.Status == "high_risk" {
		r.ConfirmedAt = r.At
		r.RiskSamples = r.Samples
	} else if r.Status != "normal" && previous != nil {
		r.ConfirmedAt = previous.ConfirmedAt
		r.RiskSamples = previous.RiskSamples
	}
	return r
}

// score only considers a completed response from the requested model. Incomplete
// output or missing response metadata is unmeasurable, not evidence of low quality.
func score(s *Sample, response Response) {
	if response.Status < 200 || response.Status >= 300 {
		s.Reason = fmt.Sprintf("上游 HTTP %d，未计为降智", response.Status)
		return
	}
	var data struct {
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
			Details      struct {
				ReasoningTokens *int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	raw := response.Body
	// Some xAI deployments return SSE even for a non-streaming request.
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "data:") || strings.HasPrefix(strings.TrimSpace(string(raw)), "event:") {
		scanner := bufio.NewScanner(strings.NewReader(string(raw)))
		scanner.Buffer(make([]byte, 4096), 2<<20)
		raw = nil
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) == nil && event.Type == "response.completed" {
				raw = event.Response
			}
		}
		if scanner.Err() != nil {
			raw = nil
		}
	}
	if json.Unmarshal(raw, &data) != nil || data.Status != "completed" || data.Model != Model {
		s.Reason = "响应未完成或未确认 grok-4.7，未计为降智"
		return
	}
	s.Model = data.Model
	s.OutputTokens = data.Usage.OutputTokens
	s.ReasoningTokens = data.Usage.Details.ReasoningTokens
	var answer strings.Builder
	for _, item := range data.Output {
		if item.Type == "message" && item.Role == "assistant" {
			for _, part := range item.Content {
				if part.Type == "output_text" {
					answer.WriteString(part.Text)
				}
			}
		}
	}
	s.Answer = strings.TrimSpace(answer.String())
	if s.Answer == "" {
		s.Reason = "缺少可判定的最终文本，未计为降智"
		return
	}
	if len(s.Answer) > 4096 {
		s.Answer = s.Answer[:4096]
	}
	value := s.Answer
	if strings.HasPrefix(value, "```json\n") && strings.HasSuffix(value, "\n```") {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "```json\n"), "\n```")
	}
	var decoded struct {
		Answer *int   `json:"answer"`
		Marker string `json:"marker"`
	}
	if json.Unmarshal([]byte(value), &decoded) != nil || decoded.Answer == nil {
		s.Status = "fail"
		s.Reason = "完成响应未给出要求的JSON整数答案"
		return
	}
	if *decoded.Answer != s.Expected {
		s.Status = "fail"
		s.Reason = "答案与程序计算结果不一致"
		return
	}
	if decoded.Marker != s.Marker {
		s.Status = "fail"
		s.Reason = "未遵循本轮校验标记要求"
		return
	}
	s.Status = "pass"
}

func Passed(r *Report, now time.Time) bool {
	return r != nil && !r.Blocked && r.Status == "normal" && r.Model == Model && r.Version == Version && r.At <= now.Unix() && now.Unix()-r.At < 86400
}
