# Local automatic workspace build

Based on cc-connect v1.5.0 (17c61062). This local patch adds project-level `workspace_auto_create = true` in multi-workspace mode. The default remains false.

On an unbound chat, the resolver creates `base_dir/chat-<128-bit hash>` from the project name and platform-qualified chat ID, persists the binding, and processes the original message. It does not depend on a channel name or ask for mkdir confirmation. Existing bindings are retained. A filesystem error stops processing without binding. This is directory organization, not an OS security boundary.

Build with the repository's normal web build and `go build -tags goolm ./cmd/cc-connect`. The npm binary is backed up before local replacement. An npm reinstall or cc-connect upgrade may replace this local feature; keep this source checkout and patch for rebuilding.

Verification:
- Observed pre-fix failures for empty-name creation, unique directories, filesystem failure, and the first-message CUJ (literal empty mkdir confirmation).
- New regression tests passed after the fix; the CUJ drives ReceiveMessage with three user actions.
- Windows full-suite failures and baseline comparison logs are retained in the parent tools directory. Do not interpret focused success as a clean full-suite run.

## Installed result

Binary: v1.5.0-local.autows. SHA256: f8ee542b83988c8a5275b2c8f57a68eed215832e9b389563362e07a6d2936692.

The npm binary was replaced and the bridge restarted; WeCom reported subscribed successfully. Configuration enables auto-create for smart-import. Parent workspace AGENTS.md retains attachment delivery instructions. No live import was triggered for verification.

Final full suite (`go test -p 2 -tags goolm ./...`) exited 1. Failed package summaries:

```text
FAIL	github.com/chenhg5/cc-connect/agent/acp	2.086s
FAIL	github.com/chenhg5/cc-connect/agent/antigravity	3.068s
FAIL	github.com/chenhg5/cc-connect/agent/claudecode	5.666s
FAIL	github.com/chenhg5/cc-connect/agent/cursor	1.841s
FAIL	github.com/chenhg5/cc-connect/agent/iflow	3.089s
FAIL	github.com/chenhg5/cc-connect/agent/kimi	3.555s
FAIL	github.com/chenhg5/cc-connect/agent/opencode	6.002s
FAIL	github.com/chenhg5/cc-connect/agent/pi	2.081s
FAIL	github.com/chenhg5/cc-connect/cmd/cc-connect [build failed]
FAIL	github.com/chenhg5/cc-connect/config	2.666s
FAIL	github.com/chenhg5/cc-connect/core	28.065s
FAIL	github.com/chenhg5/cc-connect/daemon	0.888s
FAIL	github.com/chenhg5/cc-connect/platform/cloud-web	2.957s
```

The focused workspace/CUJ/config tests and WeCom package tests passed. `go vet ./core ./config` and full executable build passed. Baseline reproduces the two AppendFileRefs Unix-path assertions and TestLoad_ResolvesEnvPlaceholders on Windows. Other failures are recorded, not claimed resolved. See ../auto-workspace-full-final.log and ../auto-workspace-baseline.log.
