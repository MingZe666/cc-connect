package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// steerTestWriter 在真实 RPC 写入点检查请求并回送响应，无需启动模型。
type steerTestWriter struct{ write func([]byte) }

// Write 将请求同步交给测试控制器。
func (w steerTestWriter) Write(b []byte) (int, error) { w.write(b); return len(b), nil }

// Close 满足管道接口，测试不拥有操作系统资源。
func (w steerTestWriter) Close() error { return nil }

// TestAppServerStartResponseAfterCompletion 防止旧响应复活已完成轮次。
func TestAppServerStartResponseAfterCompletion(t *testing.T) {
	s := &appServerSession{ctx: context.Background(), events: make(chan core.Event, 8)} // 容纳该用例的文本与终态事件。
	s.alive.Store(true)
	s.threadID.Store("thread")
	s.stdin = steerTestWriter{write: func(b []byte) {
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(b, &req)
		s.handleNotification("turn/started", []byte(`{"threadId":"thread","turn":{"id":"turn"}}`))
		s.handleNotification("item/completed", []byte(`{"threadId":"thread","turnId":"turn","item":{"type":"agentMessage","text":"done"}}`))
		s.handleNotification("turn/completed", []byte(`{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`))
		s.handleResponse(rpcResponseEnvelope{ID: req.ID, Result: json.RawMessage(`{"turn":{"id":"turn"}}`)})
	}}
	if err := s.Send("hello", "message", nil, nil); err != nil {
		t.Fatal(err)
	}
	if s.currentTurn != "" {
		t.Fatalf("completed turn revived: %s", s.currentTurn)
	}
}

// TestAppServerSteerActiveTurn 验证精确轮次和连续消息，不产生额外完成事件。
func TestAppServerSteerActiveTurn(t *testing.T) {
	s := &appServerSession{ctx: context.Background(), events: make(chan core.Event, 1), currentTurn: "turn"}
	s.alive.Store(true)
	s.threadID.Store("thread")
	var prompts []string
	s.stdin = steerTestWriter{write: func(b []byte) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				ThreadID string           `json:"threadId"`
				Expected string           `json:"expectedTurnId"`
				Input    []map[string]any `json:"input"`
			} `json:"params"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Error(err)
			return
		}
		if req.Method != "turn/steer" || req.Params.ThreadID != "thread" || req.Params.Expected != "turn" {
			t.Errorf("wrong target: %s", b)
		}
		prompts = append(prompts, req.Params.Input[0]["text"].(string))
		s.handleResponse(rpcResponseEnvelope{ID: req.ID, Result: json.RawMessage(`{"turnId":"turn"}`)})
	}}
	for _, p := range []string{"year", "format"} {
		if err := s.Steer(p, p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(prompts, []string{"year", "format"}) || len(s.events) != 0 {
		t.Fatal(prompts, len(s.events))
	}
}

// TestAppServerSteerTimeoutUnknown 验证确认超时只发送一次且结果保持未知。
func TestAppServerSteerTimeoutUnknown(t *testing.T) {
	s := &appServerSession{ctx: context.Background(), events: make(chan core.Event, 1), currentTurn: "turn", steerTimeout: time.Millisecond}
	s.alive.Store(true)
	s.threadID.Store("thread")
	writes := 0
	s.stdin = steerTestWriter{write: func([]byte) { writes++ }}
	err := s.Steer("p", "m", nil, nil)
	var se *core.SteerError
	if !errors.As(err, &se) || se.Kind != core.SteerUnknownResult || writes != 1 {
		t.Fatalf("%v writes=%d", err, writes)
	}
}

// TestAppServerCommandPreservesNetwork 验证配置覆盖参数处于子命令作用域。
func TestAppServerCommandPreservesNetwork(t *testing.T) {
	s := &appServerSession{url: "stdio://", cliExtraArgs: []string{"--enable", "example", "-c", "sandbox_workspace_write.network_access=true"}}
	args, err := s.buildAppServerArgs()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "--enable example app-server -c sandbox_workspace_write.network_access=true") {
		t.Fatal(args)
	}
}

// TestAppServerCompletionBeforeStarted 完成通知先到时必须等响应关联，不能复活轮次。
func TestAppServerCompletionBeforeStarted(t *testing.T) {
	s := &appServerSession{ctx: context.Background(), events: make(chan core.Event, 2)} // 一次终态及安全余量。
	s.alive.Store(true)
	s.threadID.Store("thread")
	s.stdin = steerTestWriter{write: func(b []byte) {
		var req struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(b, &req)
		s.handleNotification("turn/completed", []byte(`{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`))
		s.handleResponse(rpcResponseEnvelope{ID: req.ID, Result: json.RawMessage(`{"turn":{"id":"turn"}}`)})
	}}
	if err := s.Send("p", "m", nil, nil); err != nil {
		t.Fatal(err)
	}
	if s.currentTurn != "" || len(s.events) != 1 {
		t.Fatalf("turn=%s events=%d", s.currentTurn, len(s.events))
	}
}

// TestAppServerSteerErrorClassification 覆盖明确拒绝、匹配失败和成功响应身份错误。
func TestAppServerSteerErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rpc    *rpcError
		result string
		want   core.SteerErrorKind
	}{
		{"inactive", &rpcError{Code: rpcInvalidRequest, Message: "no active turn to steer"}, "", core.SteerUnavailable},
		{"mismatch", &rpcError{Code: rpcInvalidRequest, Message: "expected active turn id `turn` but found `other`"}, "", core.SteerUnavailable},
		{"invalid", &rpcError{Code: rpcInvalidRequest, Message: "bad input"}, "", core.SteerRejected},
		{"wrong response", nil, `{"turnId":"other"}`, core.SteerUnknownResult},
		{"bad response", nil, `[]`, core.SteerUnknownResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &appServerSession{ctx: context.Background(), currentTurn: "turn"}
			s.alive.Store(true)
			s.threadID.Store("thread")
			s.stdin = steerTestWriter{write: func(b []byte) {
				var req struct {
					ID int64 `json:"id"`
				}
				_ = json.Unmarshal(b, &req)
				s.handleResponse(rpcResponseEnvelope{ID: req.ID, Error: tc.rpc, Result: json.RawMessage(tc.result)})
			}}
			err := s.Steer("p", "m", nil, nil)
			var se *core.SteerError
			if !errors.As(err, &se) || se.Kind != tc.want {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// TestAppServerForeignAndDuplicateNotifications 其他轮次和 idle 不得结束当前轮。
func TestAppServerForeignAndDuplicateNotifications(t *testing.T) {
	s := &appServerSession{ctx: context.Background(), events: make(chan core.Event, 8), currentTurn: "turn"} // 存放该用例的最终输出。
	s.threadID.Store("thread")
	s.handleNotification("item/completed", []byte(`{"threadId":"other","turnId":"turn","item":{"type":"agentMessage","text":"foreign"}}`))
	s.handleNotification("turn/completed", []byte(`{"threadId":"thread","turn":{"id":"other"}}`))
	s.handleNotification("thread/status/changed", []byte(`{"threadId":"thread","status":{"type":"idle"}}`))
	s.handleNotification("item/completed", []byte(`{"threadId":"thread","turnId":"turn","item":{"type":"agentMessage","text":"answer"}}`))
	s.handleNotification("turn/started", []byte(`{"threadId":"thread","turn":{"id":"turn"}}`))
	for range 2 {
		s.handleNotification("turn/completed", []byte(`{"threadId":"thread","turn":{"id":"turn"}}`))
	} // 重复终态只发一次。
	if len(s.events) != 2 {
		t.Fatalf("events=%d", len(s.events))
	}
	if ev := <-s.events; ev.Content != "answer" {
		t.Fatal(ev)
	}
}

// TestAppServerSteerAttachments 验证公共附件构造，图片文件不会覆盖。
func TestAppServerSteerAttachments(t *testing.T) {
	s := &appServerSession{workDir: t.TempDir()}
	images := []core.ImageAttachment{{MimeType: "image/png", Data: []byte("image")}}
	first, err := s.prepareTurnInput("prompt", "m", images, []core.FileAttachment{{FileName: "report.txt", Data: []byte("report")}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.prepareTurnInput("prompt", "n", images, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first[0]["text"].(string), "report.txt") || first[1]["path"] == second[1]["path"] {
		t.Fatal(first, second)
	}
	data, err := os.ReadFile(first[1]["path"].(string))
	if err != nil || string(data) != "image" {
		t.Fatal(err, string(data))
	}
}

// TestAppServerCloseUnblocksFullEvents 取消可唤醒可靠终态发送，避免关闭通道与发送竞争。
func TestAppServerCloseUnblocksFullEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &appServerSession{ctx: ctx, cancel: cancel, events: make(chan core.Event, 1)}
	s.events <- core.Event{Type: core.EventText}
	done := make(chan struct{})
	go func() { s.emit(core.Event{Type: core.EventResult, Done: true}); close(done) }()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sender stuck")
	}
	s.emit(core.Event{Type: core.EventResult, Done: true})
}

// TestAppServerCommandHelper 仅在测试子进程等待 stdin 关闭，模拟可启动的自定义 binary。
func TestAppServerCommandHelper(t *testing.T) {
	if os.Getenv("CC_CONNECT_STEER_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// TestAppServerConnectUsesCustomCommand 实际启动指定 binary，防止只测试参数构造却漏掉 connect 接线。
func TestAppServerConnectUsesCustomCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &appServerSession{ctx: ctx, cancel: cancel, cliBin: os.Args[0], cliExtraArgs: []string{"-test.run=^TestAppServerCommandHelper$", "--"}, url: "stdio://", workDir: t.TempDir(), extraEnv: []string{"CC_CONNECT_STEER_HELPER=1"}, events: make(chan core.Event, 1)}
	s.alive.Store(true)
	if err := s.connect(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.cmd.Path != os.Args[0] {
		t.Fatalf("custom command lost: %s", s.cmd.Path)
	}
}

// TestAppServerErrorNotificationOwnership 旧轮错误和服务端重试不得中断当前轮。
func TestAppServerErrorNotificationOwnership(t *testing.T) {
	s := &appServerSession{ctx: context.Background(), events: make(chan core.Event, 1), currentTurn: "turn"} // 仅当前轮最终错误可入队。
	s.threadID.Store("thread")
	for _, raw := range []string{
		`{"threadId":"other","turnId":"turn","error":{"message":"foreign"}}`,
		`{"threadId":"thread","turnId":"old","error":{"message":"stale"}}`,
		`{"threadId":"thread","turnId":"turn","willRetry":true,"error":{"message":"retry"}}`,
		`{"message":"unscoped"}`,
	} {
		s.handleNotification("error", []byte(raw))
	}
	if len(s.events) != 0 {
		t.Fatal("unrelated error delivered")
	}
	s.handleNotification("error", []byte(`{"threadId":"thread","turnId":"turn","willRetry":false,"error":{"message":"failed"}}`))
	if len(s.events) != 0 || s.currentTurn != "turn" {
		t.Fatal("diagnostic notification prematurely ended turn")
	}
	// 诊断和重复完成通知只能生成一次终态；完成之前不能开放下一轮。
	for range 2 {
		s.handleNotification("turn/completed", []byte(`{"threadId":"thread","turn":{"id":"turn","status":"failed","error":{"message":"failed"}}}`))
	}
	if len(s.events) != 1 || s.currentTurn != "" {
		t.Fatal("completion did not settle exactly once")
	}
}
