package core

import (
	"fmt"
	"strings"
	"time"
)

// TurnSteerer 是仅向当前轮追加输入的可选会话能力，不产生新的完成事件。
type TurnSteerer interface {
	Steer(prompt, messageID string, images []ImageAttachment, files []FileAttachment) error
}

// TurnSteeringAgent 在进程创建前声明能力；其 Send 返回成功必须表示新轮已确认启动。
type TurnSteeringAgent interface {
	SupportsTurnSteering() bool
}

// SteerErrorKind 指明补充是否确定未接收，避免未知结果被自动重发。
type SteerErrorKind string

const (
	SteerUnavailable   SteerErrorKind = "unavailable"
	SteerRejected      SteerErrorKind = "rejected"
	SteerUnknownResult SteerErrorKind = "unknown"
)

// SteerError 将协议失败映射为核心层可理解的投递结果。
type SteerError struct {
	Kind  SteerErrorKind
	Cause error
}

// Error 保留带上下文的底层错误说明。
func (e *SteerError) Error() string { return fmt.Sprintf("steer %s: %v", e.Kind, e.Cause) }

// Unwrap 支持调用方检查底层原因。
func (e *SteerError) Unwrap() error { return e.Cause }

// BusyMessageMode 是项目级忙碌消息策略，零值保持原队列行为。
type BusyMessageMode string

const (
	BusyMessageQueue BusyMessageMode = "queue"
	BusyMessageSteer BusyMessageMode = "steer"
)

// ParseBusyMessageMode 同时供配置校验和启动接线使用。
func ParseBusyMessageMode(value any) (BusyMessageMode, error) {
	if value == nil {
		return BusyMessageQueue, nil
	}
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("busy_message_mode must be queue or steer")
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "queue":
		return BusyMessageQueue, nil
	case "steer":
		return BusyMessageSteer, nil
	default:
		return "", fmt.Errorf("busy_message_mode must be queue or steer")
	}
}

// SteerReceiptStatus 保存平台消息的投递状态，不作为自动重试依据。
type SteerReceiptStatus string

const (
	SteerPending         SteerReceiptStatus = "pending"
	SteerAccepted        SteerReceiptStatus = "accepted"
	SteerRejectedReceipt SteerReceiptStatus = "rejected"
	SteerUnknown         SteerReceiptStatus = "unknown"
	SteerQueued          SteerReceiptStatus = "queued"
)

// SteerReceipt 随会话持久化，以便重启后保留结果未知的消息。
type SteerReceipt struct {
	Status            SteerReceiptStatus `json:"status"`
	MessageID         string             `json:"message_id"`
	Content           string             `json:"content"`
	AgentSessionID    string             `json:"agent_session_id,omitempty"`
	Error             string             `json:"error,omitempty"`
	UserMessageTimeMs int64              `json:"user_message_time_ms,omitempty"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

// steerReceiptKey 使用分隔符保留平台和聊天边界，避免多工作区消息串用。
func steerReceiptKey(msg *Message) string {
	return msg.Platform + "\x00" + msg.SessionKey + "\x00" + msg.MessageID
}

// steerReceipt 返回记录副本，不向锁外暴露 map。
func (s *Session) steerReceipt(key string) (SteerReceipt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.SteerReceipts[key]
	return r, ok
}

// setSteerReceipt 原子记录接收状态与成功历史，重复确认不重复插入历史。
func (s *Session) setSteerReceipt(key string, r SteerReceipt, history bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SteerReceipts == nil {
		s.SteerReceipts = make(map[string]SteerReceipt)
	}
	old := s.SteerReceipts[key]
	r.UpdatedAt = time.Now()
	if history && old.Status != SteerAccepted {
		s.History = append(s.History, HistoryEntry{Role: "user", Content: r.Content, Timestamp: r.UpdatedAt})
		s.LastUserActivity = r.UpdatedAt
	}
	s.SteerReceipts[key] = r
}

// unknownSteerReceipts 返回待核实记录副本，供两种历史展示复用。
func (s *Session) unknownSteerReceipts() []SteerReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []SteerReceipt
	for _, r := range s.SteerReceipts {
		if r.Status == SteerUnknown {
			result = append(result, r)
		}
	}
	return result
}

// findSteerReceipt 在当前工作区同一聊天的历史会话中去重，切换会话不重放旧消息。
func (sm *SessionManager) findSteerReceipt(userKey, receiptKey string) (SteerReceipt, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, id := range sm.userSessions[userKey] {
		if receipt, ok := sm.sessions[id].steerReceipt(receiptKey); ok {
			return receipt, true
		}
	}
	return SteerReceipt{}, false
}
