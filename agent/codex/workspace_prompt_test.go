package codex

import (
	"strings"
	"testing"
)

// TestWorkspaceAgentOptions_PreservesProjectPrompts 验证工作区实例不会丢失报告交付等项目指令。
func TestWorkspaceAgentOptions_PreservesProjectPrompts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		system string
		append string
	}{
		{name: "both", system: "使用导账技能处理任务。", append: "报告必须作为附件发送，并验证发送成功。"},
		{name: "system_only", system: "使用导账技能处理任务。"},
		{name: "append_only", append: "报告必须作为附件发送，并验证发送成功。"},
		{name: "unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := &Agent{cmd: "go", systemPrompt: tc.system, appendPrompt: tc.append}
			opts := parent.WorkspaceAgentOptions()
			opts["work_dir"] = t.TempDir()
			// 通过真实构造函数还原工作区实例，覆盖配置快照到提示词消费的完整链路。
			created, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			child := created.(*Agent)
			if child.systemPrompt != tc.system || child.appendPrompt != tc.append {
				t.Fatalf("workspace prompts = (%q, %q), want (%q, %q)", child.systemPrompt, child.appendPrompt, tc.system, tc.append)
			}
			prompt := prependCodexPromptPreamble("生成报告", buildCodexPromptPreamble(child.systemPrompt, child.appendPrompt))
			for _, instruction := range []string{tc.system, tc.append} {
				if instruction != "" && !strings.Contains(prompt, instruction) {
					t.Fatalf("outgoing prompt missing instruction %q", instruction)
				}
			}
		})
	}
}
