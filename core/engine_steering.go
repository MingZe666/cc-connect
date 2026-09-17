package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// steerDelivery 保存独立消息副本，普通队列和实时补充不能同时拥有它。
type steerDelivery struct {
	message  Message
	platform Platform
	key      string
}

// SetBusyMessageMode 在启动组装阶段设置策略，运行中不热修改。
func (e *Engine) SetBusyMessageMode(mode BusyMessageMode) { e.busyMessageMode = mode }

// queuedFromMessage 复用两条投递路径的附件、发送者和回复上下文转换。
func queuedFromMessage(p Platform, msg *Message) queuedMessage {
	return queuedMessage{messageID: msg.MessageID, platform: p, replyCtx: msg.ReplyCtx, content: msg.Content,
		images: msg.Images, files: msg.Files, fromVoice: msg.FromVoice, userID: msg.UserID, userName: msg.UserName,
		msgPlatform: msg.Platform, msgSessionKey: msg.SessionKey, channelKey: msg.ChannelKey, userMessageTimeMs: msg.UserMessageTimeMs}
}

// handleSteerRedelivery 在空闲/忙碌分流之前去重，关闭开关后也不能重放旧未知记录。
func (e *Engine) handleSteerRedelivery(p Platform, msg *Message, sessions *SessionManager) bool {
	if msg.MessageID == "" {
		return false
	}
	receipt, ok := sessions.findSteerReceipt(msg.SessionKey, steerReceiptKey(msg))
	if !ok {
		return false
	}
	e.replySteerReceipt(p, msg.ReplyCtx, receipt)
	return true
}

// replySteerReceipt 只描述投递状态，不把待核实误报为成功。
func (e *Engine) replySteerReceipt(p Platform, ctx any, r SteerReceipt) {
	key := MsgSteerUnknown
	switch r.Status {
	case SteerAccepted:
		key = MsgSteerAccepted
	case SteerQueued:
		key = MsgMessageQueued
	case SteerPending:
		key = MsgSteerPending
	case SteerRejectedReceipt:
		e.reply(p, ctx, e.i18n.Tf(MsgSteerRejected, e.redactSteerError(r.Error)))
		return
	}
	e.reply(p, ctx, e.i18n.T(key))
}

// trySteerBusyMessage 仅登记已定位会话的补充，RPC 和平台调用始终在锁外运行。
func (e *Engine) trySteerBusyMessage(p Platform, msg *Message, key string, session *Session, sessions *SessionManager) bool {
	if e.busyMessageMode != BusyMessageSteer || msg.MessageID == "" {
		return false
	}
	e.interactiveMu.Lock()
	state := e.interactiveStates[key]
	if state == nil {
		e.interactiveMu.Unlock()
		return false
	}
	state.mu.Lock()
	e.interactiveMu.Unlock()
	if r, ok := session.steerReceipt(steerReceiptKey(msg)); ok {
		state.mu.Unlock()
		e.replySteerReceipt(p, msg.ReplyCtx, r)
		return true
	}
	_, supported := state.agentSession.(TurnSteerer)
	// 初次建连尚无 AgentSession，已声明能力的启动批次也可暂存补充。
	supported = supported || (state.steerStart != nil && state.steerStart.supported)
	if !supported || !state.steerOpen || state.stopped || len(state.pendingMessages) > 0 {
		state.mu.Unlock()
		return false
	}
	// 一个在途 RPC 不占等待容量；回退时它保留一次临时超限权，不能丢弃。
	waiting := len(state.steerQueue) + len(state.pendingMessages)
	if state.steerDone != nil && waiting >= e.maxQueuedMessages {
		state.mu.Unlock()
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgQueueFull, waiting))
		return true
	}
	copyMsg := *msg
	copyMsg.Images = append([]ImageAttachment(nil), msg.Images...)
	copyMsg.Files = append([]FileAttachment(nil), msg.Files...)
	delivery := steerDelivery{message: copyMsg, platform: p, key: steerReceiptKey(msg)}
	r := SteerReceipt{Status: SteerPending, MessageID: msg.MessageID, Content: msg.Content, UserMessageTimeMs: msg.UserMessageTimeMs, AgentSessionID: session.GetAgentSessionID()}
	session.setSteerReceipt(delivery.key, r, false)
	state.steerQueue = append(state.steerQueue, delivery)
	if state.steerDone == nil {
		state.steerDone = make(chan struct{})
		go e.runSteerAfterStart(state, key, session, sessions, state.steerStart)
	}
	state.mu.Unlock()
	return true
}

// runSteerDeliveries 是该 state 补充队列的唯一消费者和完成通道关闭者。
func (e *Engine) runSteerDeliveries(state *interactiveState, key string, session *Session, sessions *SessionManager, startErr error, gate *steerStartGate) {
	for {
		e.interactiveMu.Lock()
		// 选择当前拥有者和取队列元素共用管理锁，超时与占位迁移不能交错丢队列。
		if gate != nil {
			gate.mu.Lock()
			state = gate.state
			gate.mu.Unlock()
		}
		state.mu.Lock()
		current := e.interactiveStates[key] == state && !state.stopped && state.agentSession != nil
		e.interactiveMu.Unlock()
		if len(state.steerQueue) == 0 {
			close(state.steerDone)
			state.steerDone = nil
			state.mu.Unlock()
			return
		}
		d := state.steerQueue[0]
		state.steerQueue = state.steerQueue[1:]
		as := state.agentSession
		state.mu.Unlock()
		r, _ := session.steerReceipt(d.key)
		var err error
		if startErr != nil {
			err = &SteerError{Kind: SteerRejected, Cause: startErr}
		} else if !current {
			err = &SteerError{Kind: SteerRejected, Cause: fmt.Errorf("session ended before submission")}
		} else if saveErr := sessions.SaveWithError(); saveErr != nil {
			err = &SteerError{Kind: SteerRejected, Cause: saveErr}
		} else if !e.steerSubmissionCurrent(state, key, as) {
			err = &SteerError{Kind: SteerRejected, Cause: fmt.Errorf("session ended before submission")}
		} else {
			prompt := e.buildSenderPrompt(d.message.Content, d.message.UserID, d.message.UserName, d.message.Platform, d.message.SessionKey, d.message.ChannelKey)
			if steerer, ok := as.(TurnSteerer); ok {
				err = steerer.Steer(prompt, d.message.MessageID, d.message.Images, d.message.Files)
			} else {
				err = &SteerError{Kind: SteerUnavailable, Cause: fmt.Errorf("session does not support steering")}
			}
		}
		kind := SteerUnknownResult
		var classified *SteerError
		if errors.As(err, &classified) {
			kind = classified.Kind
		}
		if err == nil {
			r.Status = SteerAccepted
			session.setSteerReceipt(d.key, r, true)
			state.mu.Lock()
			if d.message.UserMessageTimeMs > state.currentTurnUserMessageTimeMs {
				state.currentTurnUserMessageTimeMs = d.message.UserMessageTimeMs
			}
			state.mu.Unlock()
		} else if kind == SteerUnavailable && e.fallbackSteerDeliveries(state, key, session, sessions, d) {
			continue
		} else {
			r.Status = SteerUnknown
			if kind == SteerRejected || kind == SteerUnavailable {
				r.Status = SteerRejectedReceipt
			}
			r.Error = e.redactSteerError(err.Error())
			session.setSteerReceipt(d.key, r, false)
		}
		saveErr := sessions.SaveWithError()
		if saveErr != nil {
			slog.Error("steer receipt save failed", "session", session.ID, "message_id", r.MessageID, "error", saveErr)
		}
		slog.Info("steer settled", "project", e.name, "interactive_key", key, "session", session.ID, "message_id", r.MessageID, "status", r.Status)
		// 外部回调和平台网络可能阻塞，不能让它们扣住轮次结算屏障。
		go func(d steerDelivery, r SteerReceipt, saveErr error) {
			if r.Status == SteerAccepted {
				runMessageAccepted(&d.message)
			}
			e.replySteerReceipt(d.platform, d.message.ReplyCtx, r)
			if saveErr != nil {
				e.reply(d.platform, d.message.ReplyCtx, e.i18n.T(MsgSteerSaveFailed))
			}
		}(d, r, saveErr)
	}
}

// fallbackSteerDeliveries 将旧轮补充前缀放到较晚普通消息之前，保留原登记顺序。
func (e *Engine) fallbackSteerDeliveries(state *interactiveState, key string, session *Session, sessions *SessionManager, first steerDelivery) bool {
	e.interactiveMu.Lock()
	state.mu.Lock()
	current := e.interactiveStates[key] == state && !state.stopped && state.agentSession != nil && state.agentSession.Alive()
	e.interactiveMu.Unlock()
	if !current {
		state.mu.Unlock()
		return false
	}
	batch := append([]steerDelivery{first}, state.steerQueue...)
	state.steerQueue = nil
	state.steerOpen = false
	queued := make([]queuedMessage, 0, len(batch)+len(state.pendingMessages))
	for _, d := range batch {
		q := queuedFromMessage(d.platform, &d.message)
		q.steerKey = d.key
		q.steerSession = session
		q.steerSessions = sessions
		queued = append(queued, q)
	}
	state.pendingMessages = append(queued, state.pendingMessages...)
	for _, d := range batch {
		r, _ := session.steerReceipt(d.key)
		r.Status = SteerQueued
		session.setSteerReceipt(d.key, r, false)
	}
	state.mu.Unlock()
	saveErr := sessions.SaveWithError()
	if saveErr != nil {
		slog.Error("steer fallback save failed", "error", saveErr)
	}
	for _, d := range batch {
		go func(d steerDelivery) {
			runMessageAccepted(&d.message)
			e.reply(d.platform, d.message.ReplyCtx, e.i18n.T(MsgMessageQueued))
			if saveErr != nil {
				e.reply(d.platform, d.message.ReplyCtx, e.i18n.T(MsgSteerSaveFailed))
			}
		}(d)
	}
	return true
}

// settleSteering 关闭登记后等待已提交操作分类，不持有数据锁等待 RPC。
func (e *Engine) settleSteering(state *interactiveState) {
	state.mu.Lock()
	state.steerOpen = false
	done := state.steerDone
	state.mu.Unlock()
	if done != nil {
		<-done
	}
}

// openSteering 为每轮建立独立确认信号，Send 返回之前补充只登记不发送。
func (e *Engine) openSteering(state *interactiveState) {
	state.mu.Lock()
	_, supported := state.agentSession.(TurnSteerer)
	state.steerStart = &steerStartGate{done: make(chan struct{}), state: state, supported: supported}
	state.steerOpen = true
	state.mu.Unlock()
}

// recordQueuedHistory 在回退消息实际开始下一轮时同步更新接收记录。
func recordQueuedHistory(session *Session, q queuedMessage) {
	if q.steerKey == "" {
		session.AddHistory("user", q.content)
		return
	}
	r, _ := session.steerReceipt(q.steerKey)
	r.Status = SteerPending
	session.setSteerReceipt(q.steerKey, r, true)
}

// steerHistorySummary 在对话历史之外显示待核实项，不把这些内容送给模型。
func (e *Engine) steerHistorySummary(session *Session) string {
	records := session.unknownSteerReceipts()
	sort.Slice(records, func(i, j int) bool { return records[i].UpdatedAt.Before(records[j].UpdatedAt) })
	var b strings.Builder
	for _, r := range records {
		b.WriteString("\n" + e.i18n.Tf(MsgSteerHistoryUnknown, r.MessageID, truncateHistoryEntry(r.Content, e.historyEntryMaxLen())) + "\n")
	}
	return b.String()
}

// redactSteerError 复用现有令牌替换逻辑，避免 provider 密钥进入记录和平台回复。
func (e *Engine) redactSteerError(text string) string {
	if providers, ok := e.agent.(ProviderSwitcher); ok {
		for _, provider := range providers.ListProviders() {
			text = RedactToken(text, provider.APIKey)
			for _, value := range provider.CodexHTTPHeaders {
				text = RedactToken(text, value)
			}
		}
	}
	return text
}

// steerSubmissionCurrent 是提交与停止的顺序边界，必须在可能阻塞的保存之后执行。
func (e *Engine) steerSubmissionCurrent(state *interactiveState, key string, as AgentSession) bool {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	state.mu.Lock()
	defer state.mu.Unlock()
	return e.interactiveStates[key] == state && !state.stopped && state.agentSession == as && as.Alive()
}

// sendQueuedMessage 在普通 Send 真正返回后确认回退记录，崩溃前 Pending 恢复为未知。
func (e *Engine) sendQueuedMessage(as AgentSession, session *Session, sessions *SessionManager, q queuedMessage, prompt string) error {
	if as == nil {
		err := fmt.Errorf("agent session ended before submission")
		e.rejectQueuedSteer(q, err)
		return err
	}
	if q.steerKey != "" {
		if err := sessions.SaveWithError(); err != nil {
			// 保存失败时确定尚未发送，必须结算状态以免永久显示提交中。
			e.rejectQueuedSteer(q, err)
			return err
		}
	}
	err := as.Send(prompt, q.messageID, q.images, q.files)
	if q.steerKey != "" {
		r, _ := session.steerReceipt(q.steerKey)
		r.Status = SteerAccepted
		if err != nil {
			r.Status = SteerUnknown
			r.Error = e.redactSteerError(err.Error())
		}
		session.setSteerReceipt(q.steerKey, r, false)
		if saveErr := sessions.SaveWithError(); saveErr != nil {
			slog.Error("queued steer save failed", "error", saveErr)
		}
	}
	return err
}

// rejectQueuedSteer 队列被撤销时标记明确未提交，不再宣称它还在等待执行。
func (e *Engine) rejectQueuedSteer(q queuedMessage, reason error) {
	if q.steerKey == "" {
		return
	}
	r, _ := q.steerSession.steerReceipt(q.steerKey)
	r.Status = SteerRejectedReceipt
	r.Error = e.redactSteerError(reason.Error())
	q.steerSession.setSteerReceipt(q.steerKey, r, false)
	if err := q.steerSessions.SaveWithError(); err != nil {
		slog.Error("dropped steer save failed", "error", err)
	}
}

// discardQueuedSteering 静默停止仍更新记录，只省略平台通知。
func (e *Engine) discardQueuedSteering(state *interactiveState) {
	state.mu.Lock()
	batch := state.pendingMessages
	state.pendingMessages = nil
	state.mu.Unlock()
	for _, q := range batch {
		e.rejectQueuedSteer(q, fmt.Errorf("session stopped before submission"))
	}
}

// steerStartWaitTimeout 限制本地补充等待启动的时间；超时仅拒绝尚未提交的补充。
const steerStartWaitTimeout = time.Minute

// steerStartGate 的实例身份绑定一个启动批次，跨占位 state 迁移仍属于同一轮。
type steerStartGate struct {
	claimed   bool // state.mu 保护，表示本批次已交给 Send。
	supported bool // 创建前声明或实际会话提供的能力，创建后不再修改。
	once      sync.Once
	mu        sync.Mutex
	done      chan struct{}
	err       error
	state     *interactiveState
}

// finish 由发送完成、启动失败或停止路径结算，只有首次结果生效。
func (g *steerStartGate) finish(err error) {
	if g != nil {
		g.once.Do(func() { g.err = err; close(g.done) })
	}
}

// runSteerAfterStart 不持状态锁等待启动；迁移完成后获取该批次实际拥有者。
func (e *Engine) runSteerAfterStart(state *interactiveState, key string, session *Session, sessions *SessionManager, gate *steerStartGate) {
	var startErr error
	if gate != nil {
		startErr = gate.wait(e.ctx, steerStartWaitTimeout)
	}
	e.runSteerDeliveries(state, key, session, sessions, startErr, gate)
}

// prepareSteeringStart 在异步创建进程前登记能力，覆盖 /new 后首条消息启动窗口。
func (e *Engine) prepareSteeringStart(key string, agent Agent) {
	capable, ok := agent.(TurnSteeringAgent)
	if e.busyMessageMode != BusyMessageSteer || !ok || !capable.SupportsTurnSteering() {
		return
	}
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	state := e.interactiveStates[key]
	e.openSteering(state)
	state.mu.Lock()
	state.steerStart.supported = true
	state.mu.Unlock()
}

// sendWithSteeringReady 捕获当前批次，旧 Send 的迟到返回不会放行新轮补充。
func (e *Engine) sendWithSteeringReady(gate *steerStartGate, send func() error) error {
	err := send()
	gate.finish(err)
	return err
}

// wait 的超时只说明补充未提交；不能据此重发主任务，也不能让迟到成功覆盖结论。
func (g *steerStartGate) wait(ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-g.done:
	case <-ctx.Done():
		g.finish(ctx.Err())
	case <-timer.C:
		g.finish(fmt.Errorf("turn startup confirmation timed out; addition not submitted"))
	}
	<-g.done
	return g.err
}
