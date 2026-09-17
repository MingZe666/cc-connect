package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSteerReceiptsSurviveRestart 验证未确认消息不会在恢复时被视为可以重发。
func TestSteerReceiptsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	sm := NewSessionManager(path)
	s := sm.GetOrCreateActive("chat")
	for _, status := range []SteerReceiptStatus{SteerPending, SteerAccepted, SteerQueued, SteerUnknown} {
		s.setSteerReceipt(string(status), SteerReceipt{Status: status, MessageID: string(status)}, false)
	}
	if err := sm.SaveWithError(); err != nil {
		t.Fatal(err)
	}
	restored := NewSessionManager(path).GetOrCreateActive("chat")
	for _, key := range []SteerReceiptStatus{SteerPending, SteerQueued, SteerUnknown} {
		r, ok := restored.steerReceipt(string(key))
		if !ok || r.Status != SteerUnknown {
			t.Fatalf("%s: %#v", key, r)
		}
	}
	r, _ := restored.steerReceipt(string(SteerAccepted))
	if r.Status != SteerAccepted {
		t.Fatal(r)
	}
}

// TestSteerReceiptSaveFailure 验证调用方能发现持久化失败而停止提交。
func TestSteerReceiptSaveFailure(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	} // 仅当前用户可读写的测试文件。
	sm := NewSessionManager("")
	sm.storePath = filepath.Join(blocker, "sessions.json")
	if err := sm.SaveWithError(); err == nil {
		t.Fatal("expected save error")
	}
}

// TestSteerErrorUnwrap 保留底层错误以便明确区分业务拒绝与传输失败。
func TestSteerErrorUnwrap(t *testing.T) {
	cause := errors.New("transport")
	err := &SteerError{Kind: SteerUnknownResult, Cause: cause}
	if !errors.Is(err, cause) {
		t.Fatal("missing cause")
	}
}
