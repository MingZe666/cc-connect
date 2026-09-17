package codex

import (
	"reflect"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestWorkspaceAgentOptions_PreservesCommandAndNetworkArgs(t *testing.T) {
	for _, command := range []any{
		"codex -c sandbox_workspace_write.network_access=true",
		[]string{`C:\Program Files\Codex\codex.exe`, "-c", "sandbox_workspace_write.network_access=true"},
		"codex",
	} {
		bin, args := core.ParseCmdOpts(map[string]any{"cmd": command}, "codex")
		parent := &Agent{cmd: bin, cliExtraArgs: args, mode: "full-auto", backend: "app_server", appServerURL: "stdio://", model: "model-test", reasoningEffort: "high"}
		opts := parent.WorkspaceAgentOptions()
		// 后端、模型与网络参数必须作为同一份工作区选项保留。
		for key, want := range map[string]string{"backend": "app_server", "app_server_url": "stdio://", "model": "model-test", "reasoning_effort": "high"} {
			if opts[key] != want {
				t.Fatalf("%s=%v", key, opts[key])
			}
		}
		childBin, childArgs := core.ParseCmdOpts(opts, "codex")
		if childBin != bin || !reflect.DeepEqual(childArgs, args) {
			t.Fatalf("workspace command = %q %v, want %q %v", childBin, childArgs, bin, args)
		}
		if len(childArgs) > 0 {
			childArgs[0] = "modified"
			if parent.cliExtraArgs[0] == "modified" {
				t.Fatal("workspace arguments alias parent arguments")
			}
		}
	}
}

func TestBuildExecArgs_ConfigOverridesReachSubcommand(t *testing.T) {
	for _, tid := range []string{"", "existing-session"} {
		cs := &codexSession{mode: "full-auto", cliExtraArgs: []string{"--enable", "example", "-c", "sandbox_workspace_write.network_access=true", "--config=example=true", "--config", "other=true"}}
		cs.threadID.Store(tid)
		args := cs.buildExecArgs("hello", nil)
		want := []string{"--enable", "example", "exec"}
		if tid != "" {
			want = append(want, "resume")
		}
		want = append(want, "--skip-git-repo-check", "-c", "sandbox_workspace_write.network_access=true", "--config=example=true", "--config", "other=true")
		if len(args) < len(want) || !reflect.DeepEqual(args[:len(want)], want) {
			t.Fatalf("config overrides must be in exec/resume scope: got %v, want prefix %v", args, want)
		}
	}
}
