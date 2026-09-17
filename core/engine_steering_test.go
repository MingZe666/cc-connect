package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// steerControlledSession 通过显式屏障控制 RPC 确认顺序，复用基础会话桩。
type steerControlledSession struct {
	stubAgentSession
	entered chan string
	result  chan error
}

// Steer 在测试允许前不返回，模拟等待远端确认。
func (s *steerControlledSession) Steer(prompt, id string, _ []ImageAttachment, _ []FileAttachment) error {
	s.entered <- id
	return <-s.result
}

// TestSteerCompletionBarrier 验证完成事件早到时等待确认，并只记录一次成功历史。
func TestSteerCompletionBarrier(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetBusyMessageMode(BusyMessageSteer)
	p := &stubPlatformEngine{n: "test"}
	s := e.sessions.GetOrCreateActive("chat")
	as := &steerControlledSession{entered: make(chan string, 1), result: make(chan error, 1)}
	state := &interactiveState{agentSession: as, steerOpen: true}
	e.interactiveStates["chat"] = state
	msg := &Message{Platform: "test", SessionKey: "chat", MessageID: "m", Content: "2026"}
	if !e.trySteerBusyMessage(p, msg, "chat", s, e.sessions) {
		t.Fatal("not handled")
	}
	select {
	case <-as.entered:
	case <-time.After(time.Second):
		t.Fatal("no steer")
	}
	done := make(chan struct{})
	go func() { e.settleSteering(state); close(done) }()
	select {
	case <-done:
		t.Fatal("completed before ack")
	default:
	}
	as.result <- nil
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("barrier stuck")
	}
	if got := s.GetHistory(0); len(got) != 1 || got[0].Content != "2026" {
		t.Fatal(got)
	}
	if !e.handleSteerRedelivery(p, msg, e.sessions) {
		t.Fatal("duplicate not recognized")
	}
	if len(state.pendingMessages) != 0 {
		t.Fatal("accepted message queued")
	}
}

// TestSteerUnavailablePreservesOrder 即使后续消息先入普通队列，回退仍保持原顺序。
func TestSteerUnavailablePreservesOrder(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetBusyMessageMode(BusyMessageSteer)
	p := &stubPlatformEngine{n: "test"}
	s := e.sessions.GetOrCreateActive("chat")
	as := &steerControlledSession{entered: make(chan string, 1), result: make(chan error, 1)}
	state := &interactiveState{agentSession: as, steerOpen: true}
	e.interactiveStates["chat"] = state
	for _, id := range []string{"a", "b"} {
		if !e.trySteerBusyMessage(p, &Message{Platform: "test", SessionKey: "chat", MessageID: id, Content: id}, "chat", s, e.sessions) {
			t.Fatal("not handled")
		}
		if id == "a" {
			<-as.entered
		}
	}
	state.mu.Lock()
	state.steerOpen = false
	state.mu.Unlock()
	e.queueMessageForBusySession(p, &Message{SessionKey: "chat", MessageID: "c", Content: "c"}, "chat")
	as.result <- &SteerError{Kind: SteerUnavailable, Cause: fmt.Errorf("ended")}
	e.settleSteering(state)
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.pendingMessages) != 3 {
		t.Fatal(len(state.pendingMessages))
	}
	for i, want := range []string{"a", "b", "c"} {
		if state.pendingMessages[i].messageID != want {
			t.Fatal(state.pendingMessages)
		}
	}
}

// steerJourneySession 只模拟模型进程，Engine 和会话持久化使用真实实现。
type steerJourneySession struct {
	stubAgentSession
	events    chan Event
	started   chan string
	mu        sync.Mutex
	additions []string
	steerErr  error
}

// Send 记录新轮开始，但由测试控制实际完成时刻。
func (s *steerJourneySession) Send(prompt, id string, _ []ImageAttachment, _ []FileAttachment) error {
	s.started <- id
	return nil
}

// Events 提供跨轮次保持开启的事件流。
func (s *steerJourneySession) Events() <-chan Event { return s.events }

// Steer 仅修改当前轮的模型输入，不创建第二轮。
func (s *steerJourneySession) Steer(prompt, id string, _ []ImageAttachment, _ []FileAttachment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.additions = append(s.additions, prompt)
	return s.steerErr
}

// finish 把模拟模型看到的补充内容返回到用户平台。
func (s *steerJourneySession) finish() {
	s.mu.Lock()
	text := "report: " + strings.Join(s.additions, " | ")
	s.mu.Unlock()
	s.events <- Event{Type: EventResult, Content: text, Done: true}
}

// newSteerJourney 创建真实入口测试环境，缓冲只容纳本测试的少量轮次。
func newSteerJourney(t *testing.T) (*cujEnv, *steerJourneySession) {
	t.Helper()
	s := &steerJourneySession{events: make(chan Event, 8), started: make(chan string, 8)} // 允许测试检查意外产生的额外轮次。
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &resultAgent{session: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetBusyMessageMode(BusyMessageSteer)
	t.Cleanup(func() { e.cancel() })
	return &cujEnv{t: t, engine: e, plat: p}, s
}

// sendSteerJourney 从平台真实入口发送消息，消息标识固定以便模拟重投。
func sendSteerJourney(env *cujEnv, id, content string) {
	env.engine.ReceiveMessage(env.plat, &Message{Platform: "test", SessionKey: "test:chat", UserID: "user", MessageID: id, Content: content})
}

// waitSteerReply 等待用户可见文字，而不依赖 Engine 内部状态。
func waitSteerReply(env *cujEnv, text string) {
	env.waitFor(text, 3*time.Second, func() bool { return strings.Contains(strings.Join(env.plat.getSent(), "\n"), text) })
} // 平台异步回复的测试上限。

// TestSteerConcurrentRedelivery 同 ID 并发到达时只能有一条补充投递。
func TestSteerConcurrentRedelivery(t *testing.T) {
	env, as := newSteerJourney(t)
	sendSteerJourney(env, "start", "report")
	select {
	case <-as.started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	var wg sync.WaitGroup
	const deliveries = 12 // 模拟平台短时间重复回调。
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); sendSteerJourney(env, "same", "only 2026") }()
	}
	wg.Wait()
	waitSteerReply(env, "Added to the current task")
	as.mu.Lock()
	count := len(as.additions)
	as.mu.Unlock()
	if count != 1 {
		t.Fatalf("steer calls=%d", count)
	}
	as.finish()
	waitSteerReply(env, "report:")
}

// TestSteerUnknownSurvivesIdleRestart 重建 Engine 后同 ID 也不能经普通 Send 重投。
func TestSteerUnknownSurvivesIdleRestart(t *testing.T) {
	env, as := newSteerJourney(t)
	session := env.engine.sessions.GetOrCreateActive("test:chat")
	msg := &Message{Platform: "test", SessionKey: "test:chat", MessageID: "m", Content: "year"}
	session.setSteerReceipt(steerReceiptKey(msg), SteerReceipt{Status: SteerUnknown, MessageID: "m", Content: "year"}, false)
	if err := env.engine.sessions.SaveWithError(); err != nil {
		t.Fatal(err)
	}
	restarted := NewEngine("test", &resultAgent{session: as}, []Platform{env.plat}, env.engine.sessions.StorePath(), LangEnglish)
	defer restarted.cancel()
	restarted.ReceiveMessage(env.plat, msg)
	waitSteerReply(env, "not resent")
	select {
	case <-as.started:
		t.Fatal("replayed after restart")
	default:
	}
}

// TestSteerZeroCapacityFallback 在途补充明确未接收时保留一次回退权。
func TestSteerZeroCapacityFallback(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetBusyMessageMode(BusyMessageSteer)
	e.SetMaxQueuedMessages(0)
	p := &stubPlatformEngine{n: "test"}
	session := e.sessions.GetOrCreateActive("chat")
	as := &steerControlledSession{entered: make(chan string, 1), result: make(chan error, 1)}
	state := &interactiveState{agentSession: as, steerOpen: true}
	e.interactiveStates["chat"] = state
	e.trySteerBusyMessage(p, &Message{Platform: "test", SessionKey: "chat", MessageID: "m"}, "chat", session, e.sessions)
	<-as.entered
	as.result <- &SteerError{Kind: SteerUnavailable, Cause: fmt.Errorf("ended")}
	e.settleSteering(state)
	if len(state.pendingMessages) != 1 {
		t.Fatal("fallback lost")
	}
	e.notifyDroppedQueuedMessages(state, fmt.Errorf("stopped"))
	r, _ := session.steerReceipt("test\x00chat\x00m")
	if r.Status != SteerRejectedReceipt {
		t.Fatal(r)
	}
}

// steerWorkspaceAgent 让多工作区工厂仍使用真实 Engine 的 Agent 注册机制。
type steerWorkspaceAgent struct {
	resultAgent
	name string
}

// Name 使用此测试专属注册名，不影响其他测试的 stub 工厂。
func (a *steerWorkspaceAgent) Name() string { return a.name }

// TestSteerQueuedSaveFailureSettlesReceipt 确定未发送的失败必须退出提交中状态。
func TestSteerQueuedSaveFailureSettlesReceipt(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	s := e.sessions.GetOrCreateActive("chat")
	q := queuedMessage{steerKey: "receipt", steerSession: s, steerSessions: e.sessions}
	s.setSteerReceipt(q.steerKey, SteerReceipt{Status: SteerPending}, false)
	as := &steerJourneySession{started: make(chan string, 1)} // 容纳一次意外发送，便于明确断言。
	if err := e.sendQueuedMessage(as, s, e.sessions, q, "prompt"); err == nil {
		t.Fatal("expected persistence failure")
	}
	r, _ := s.steerReceipt(q.steerKey)
	if r.Status != SteerRejectedReceipt || len(as.started) != 0 {
		t.Fatalf("receipt=%+v sent=%d", r, len(as.started))
	}
}

// TestSteerReceiptLookupStaysWithinChat 跨本地会话去重仍不得越过聊天边界。
func TestSteerReceiptLookupStaysWithinChat(t *testing.T) {
	sm := NewSessionManager("")
	s := sm.GetOrCreateActive("chat")
	s.setSteerReceipt("key", SteerReceipt{Status: SteerUnknown}, false)
	sm.NewSession("chat", "new")
	if _, ok := sm.findSteerReceipt("chat", "key"); !ok {
		t.Fatal("lost old receipt")
	}
	if _, ok := sm.findSteerReceipt("other", "key"); ok {
		t.Fatal("cross-chat receipt")
	}
}

// TestSteerWaitsForSendReady 验证每一轮都等待启动确认，不能抢在 Send 返回前补充。
func TestSteerWaitsForSendReady(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	defer e.cancel()
	e.SetBusyMessageMode(BusyMessageSteer)
	p := &stubPlatformEngine{n: "test"}
	s := e.sessions.GetOrCreateActive("chat")
	as := &steerControlledSession{entered: make(chan string, 1), result: make(chan error, 1)} // 每次仅一条在途补充。
	state := &interactiveState{agentSession: as}
	e.interactiveStates["chat"] = state
	for _, id := range []string{"first", "next"} {
		e.openSteering(state)
		if !e.trySteerBusyMessage(p, &Message{Platform: "test", SessionKey: "chat", MessageID: id}, "chat", s, e.sessions) {
			t.Fatal("not registered")
		}
		select {
		case <-as.entered:
			t.Fatal("steer submitted before turn/start confirmation")
		case <-time.After(30 * time.Millisecond): // 短观察窗口用于暴露旧实现的抢跑。
		}
		state.steerStart.finish(nil)
		select {
		case <-as.entered:
		case <-time.After(time.Second):
			t.Fatal("not released")
		}
		as.result <- nil
		e.settleSteering(state)
	}
}

// TestSteerStartupPlaceholderTransfer 验证 /new 首轮占位状态替换不会丢失暂存附件。
func TestSteerStartupPlaceholderTransfer(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	defer e.cancel()
	e.SetBusyMessageMode(BusyMessageSteer)
	p := &stubPlatformEngine{n: "test"}
	s := e.sessions.GetOrCreateActive("chat")
	placeholder := &interactiveState{}
	e.interactiveStates["chat"] = placeholder
	e.openSteering(placeholder)
	placeholder.steerStart.supported = true // 模拟工厂已声明 App Server 能力。
	for _, id := range []string{"file2", "file3", "instructions"} {
		if !e.trySteerBusyMessage(p, &Message{Platform: "test", SessionKey: "chat", MessageID: id, Content: id}, "chat", s, e.sessions) {
			t.Fatal("startup addition queued")
		}
	}
	as := &steerControlledSession{entered: make(chan string, 1), result: make(chan error, 1)} // 单 worker 顺序提交。
	state := &interactiveState{agentSession: as}
	e.interactiveMu.Lock()
	adoptPendingFromPlaceholder(placeholder, state)
	e.interactiveStates["chat"] = state
	e.interactiveMu.Unlock()
	state.steerStart.finish(nil)
	for _, want := range []string{"file2", "file3", "instructions"} {
		select {
		case got := <-as.entered:
			if got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("addition lost")
		}
		as.result <- nil
	}
	e.settleSteering(state)
	if len(state.pendingMessages) != 0 {
		t.Fatal("submitted addition also queued")
	}
}

// TestSteerStartupFailureDoesNotSubmit 启动失败或停止必须唤醒补充并明确标记未提交。
func TestSteerStartupFailureDoesNotSubmit(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			e := NewEngine("test", &stubAgent{}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
			defer e.cancel()
			e.SetBusyMessageMode(BusyMessageSteer)
			p := &stubPlatformEngine{n: "test"}
			s := e.sessions.GetOrCreateActive("chat")
			as := &steerControlledSession{entered: make(chan string, 1), result: make(chan error, 1)} // 意外调用可观察。
			state := &interactiveState{agentSession: as}
			e.interactiveStates["chat"] = state
			e.openSteering(state)
			msg := &Message{Platform: "test", SessionKey: "chat", MessageID: "addition"}
			e.trySteerBusyMessage(p, msg, "chat", s, e.sessions)
			if stop {
				state.markStopped()
			} else {
				state.steerStart.finish(fmt.Errorf("start failed"))
			}
			e.settleSteering(state)
			r, _ := s.steerReceipt(steerReceiptKey(msg))
			if len(as.entered) != 0 || r.Status != SteerRejectedReceipt {
				t.Fatalf("sent=%d receipt=%+v", len(as.entered), r)
			}
		})
	}
}

// delayedSteerJourney 模拟服务端正在分配 turnId，测试线程显式放行启动响应。
type delayedSteerJourney struct {
	steerJourneySession
	ready chan struct{}
}

// Send 的返回与真实 Codex 一致，代表启动已确认而非整轮任务完成。
func (s *delayedSteerJourney) Send(prompt, id string, images []ImageAttachment, files []FileAttachment) error {
	s.started <- id
	<-s.ready
	return nil
}

// startupSteerAgent 在进程创建之前声明实时补充能力。
type startupSteerAgent struct{ resultAgent }

// SupportsTurnSteering 使真实入口在首条消息启动时创建等待批次。
func (*startupSteerAgent) SupportsTurnSteering() bool { return true }

// TestSteerStartupTimeoutIsFinal 超时批次不得被迟到的启动成功重新开放。
func TestSteerStartupTimeoutIsFinal(t *testing.T) {
	gate := &steerStartGate{done: make(chan struct{})}
	const waitLimit = time.Millisecond // 短超时使测试覆盖真实定时器分支。
	if err := gate.wait(context.Background(), waitLimit); err == nil {
		t.Fatal("expected timeout")
	}
	gate.finish(nil)
	if gate.err == nil {
		t.Fatal("late success revived timed-out startup")
	}
}

// TestSteerWorkerUsesTransferredOwner 超时后才完成迁移时，旧 worker 必须消费新拥有者的队列。
func TestSteerWorkerUsesTransferredOwner(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	defer e.cancel()
	s := e.sessions.GetOrCreateActive("chat")
	p := &stubPlatformEngine{n: "test"}
	old := &interactiveState{}
	e.openSteering(old)
	msg := Message{Platform: "test", SessionKey: "chat", MessageID: "m"}
	key := steerReceiptKey(&msg)
	s.setSteerReceipt(key, SteerReceipt{Status: SteerPending}, false)
	old.steerQueue = []steerDelivery{{message: msg, platform: p, key: key}}
	old.steerDone = make(chan struct{})
	gate := old.steerStart
	gate.finish(fmt.Errorf("startup timed out"))
	current := &interactiveState{}
	e.interactiveMu.Lock()
	adoptPendingFromPlaceholder(old, current)
	e.interactiveStates["chat"] = current
	e.interactiveMu.Unlock()
	e.runSteerDeliveries(old, "chat", s, e.sessions, gate.err, gate)
	r, _ := s.steerReceipt(key)
	if r.Status != SteerRejectedReceipt || len(current.steerQueue) != 0 || current.steerDone != nil {
		t.Fatal("transferred queue stranded")
	}
}

// TestSteerOldSendCannotReleaseNewGate 迟到旧响应只能完成发起时捕获的批次。
func TestSteerOldSendCannotReleaseNewGate(t *testing.T) {
	e := &Engine{}
	state := &interactiveState{}
	e.openSteering(state)
	old := state.steerStart
	e.openSteering(state)
	if err := e.sendWithSteeringReady(old, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-state.steerStart.done:
		t.Fatal("old send released new startup")
	default:
	}
}
