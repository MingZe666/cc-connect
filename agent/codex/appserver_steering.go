package codex

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// appServerSteerTimeout 限制补充确认等待，避免轮次结算被长期挂起。
const appServerSteerTimeout = 10 * time.Second

// rpcInvalidRequest 是 JSON-RPC 的无效请求码，必须同时匹配已验证的错误文本。
const rpcInvalidRequest = -32600

// Error 保留服务端错误类型，让调用方与本地传输错误区分。
func (e *rpcError) Error() string { return e.Message }

// prepareTurnInput 复用文件、图片保存与输入构造，避免 start/steer 附件行为分叉。
func (s *appServerSession) prepareTurnInput(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) ([]map[string]any, error) {
	workDir := s.GetWorkDir()
	if len(files) > 0 {
		paths := core.SaveFilesToDisk(workDir, messageID, files)
		if len(paths) != len(files) {
			return nil, fmt.Errorf("codex app-server: attachment save incomplete")
		}
		prompt = core.AppendFileRefs(prompt, paths)
	}
	prompt, paths, err := s.stageImages(prompt, images)
	if err != nil {
		return nil, err
	}
	input := []map[string]any{{"type": "text", "text": prompt, "text_elements": []any{}}}
	for _, path := range paths {
		input = append(input, map[string]any{"type": "localImage", "path": path})
	}
	return input, nil
}

// Steer 只提交到捕获的当前轮次；不会启动轮次、修改原输出缓冲或自动重试。
func (s *appServerSession) Steer(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !s.Alive() {
		return &core.SteerError{Kind: core.SteerRejected, Cause: fmt.Errorf("session closed before submission")}
	}
	s.stateMu.Lock()
	turnID := s.currentTurn
	threadID := s.CurrentSessionID()
	s.stateMu.Unlock()
	if turnID == "" {
		return &core.SteerError{Kind: core.SteerUnavailable, Cause: fmt.Errorf("no active turn")}
	}
	input, err := s.prepareTurnInput(prompt, messageID, images, files)
	if err != nil {
		return &core.SteerError{Kind: core.SteerRejected, Cause: err}
	}
	timeout := s.steerTimeout
	if timeout == 0 {
		timeout = appServerSteerTimeout
	}
	slog.Debug("codex steer submission", "message_id", messageID, "thread_id", threadID, "expected_turn_id", turnID)
	var response struct {
		TurnID string `json:"turnId"`
	}
	err = s.requestWithTimeout("turn/steer", map[string]any{"threadId": threadID, "expectedTurnId": turnID, "input": input}, &response, timeout)
	if err != nil {
		kind := core.SteerUnknownResult
		var rpc *rpcError
		if errors.As(err, &rpc) {
			kind = core.SteerRejected
			// 由本机 0.154.0-alpha.6.2 隔离实例验证；不可泛化到所有 -32600。
			if rpc.Code == rpcInvalidRequest && (rpc.Message == "no active turn to steer" || matchesExpectedTurnMismatch(rpc.Message, turnID)) {
				kind = core.SteerUnavailable
			}
		}
		return &core.SteerError{Kind: kind, Cause: err}
	}
	if response.TurnID != turnID {
		return &core.SteerError{Kind: core.SteerUnknownResult, Cause: fmt.Errorf("codex app-server: steer response turn mismatch")}
	}
	return nil
}

// matchesTurn 只允许目标 thread 和当前 turn 的事件修改会话。
func (s *appServerSession) matchesTurn(threadID, turnID string) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return threadID != "" && threadID == s.CurrentSessionID() && turnID != "" && turnID == s.currentTurn
}

// acceptStartedTurn 在同一读循环接纳开始通知或响应，完成过的轮次不能被复活。
func (s *appServerSession) acceptStartedTurn(threadID, turnID string, generation uint64) {
	s.stateMu.Lock()
	if threadID != s.CurrentSessionID() || turnID == "" || s.completedTurns[turnID] || (generation != 0 && generation != s.generation) {
		s.stateMu.Unlock()
		return
	}
	if s.currentTurn == turnID || !s.starting || s.currentTurn != "" {
		s.stateMu.Unlock()
		return
	}
	s.currentTurn = turnID
	s.pendingMsgs = nil
	early, completed := s.earlyCompletions[turnID]
	s.stateMu.Unlock()
	s.storeContextUsage(nil)
	if completed {
		s.handleTurnCompleted(early)
	}
}

// handleTurnCompleted 记录先到的终态，只有关联本地启动后才能完成。
func (s *appServerSession) handleTurnCompleted(n turnNotification) {
	s.stateMu.Lock()
	if n.ThreadID != s.CurrentSessionID() || n.Turn.ID == "" || s.completedTurns[n.Turn.ID] {
		s.stateMu.Unlock()
		return
	}
	if s.currentTurn == "" && s.starting {
		if s.earlyCompletions == nil {
			s.earlyCompletions = make(map[string]turnNotification)
		}
		s.earlyCompletions[n.Turn.ID] = n
		s.stateMu.Unlock()
		return
	}
	matches := s.currentTurn == n.Turn.ID
	s.stateMu.Unlock()
	if matches {
		if n.Turn.Status == "failed" {
			message := "codex turn failed"
			if n.Turn.Error != nil {
				message = n.Turn.Error.Message
			}
			s.completeTurn(fmt.Errorf("codex app-server: %s", message))
			return
		}
		s.completeTurn()
	}
}

// buildAppServerArgs 保留命令参数并将配置覆盖放入 app-server 作用域。
func (s *appServerSession) buildAppServerArgs() ([]string, error) {
	args := []string{"app-server"}
	var root []string
	for i := 0; i < len(s.cliExtraArgs); i++ {
		arg := s.cliExtraArgs[i]
		if arg == "--listen" || strings.HasPrefix(arg, "--listen=") || arg == "--stdio" {
			return nil, fmt.Errorf("codex app-server: configure transport using app_server_url")
		}
		if arg == "-c" || arg == "--config" {
			if i+1 == len(s.cliExtraArgs) {
				return nil, fmt.Errorf("codex app-server: missing config value")
			}
			args = append(args, arg, s.cliExtraArgs[i+1])
			i++
		} else if strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "-c") {
			args = append(args, arg)
		} else {
			root = append(root, arg)
		}
	}
	args = append(root, args...)
	if strings.TrimSpace(s.url) != "" {
		args = append(args, "--listen", strings.TrimSpace(s.url))
	}
	for _, option := range []struct{ key, value string }{{"model", s.model}, {"model_reasoning_effort", s.effort}, {"model_provider", s.modelProvider}, {"openai_base_url", s.baseURL}} {
		if value := strings.TrimSpace(option.value); value != "" {
			args = append(args, "-c", fmt.Sprintf("%s=%q", option.key, value))
		}
	}
	return args, nil
}

// matchesExpectedTurnMismatch 只匹配目标 CLI 实测的完整错误形状和本次 expected ID。
func matchesExpectedTurnMismatch(message, expected string) bool {
	tail, ok := strings.CutPrefix(message, "expected active turn id `"+expected+"` but found `")
	if !ok || !strings.HasSuffix(tail, "`") {
		return false
	}
	found := strings.TrimSuffix(tail, "`")
	return found != "" && found != expected && !strings.ContainsAny(found, "`\r\n")
}
