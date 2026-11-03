package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "终止一条正在运行的意图(work)。用于叫停跑偏/无意义的探索；被终止的意图标记为 stopped，不再自动重领。先用 get_worker_output 看看它在干嘛再决定。",
		obj(map[string]any{"intent_id": idp("要终止的意图 id（= work 句柄）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work 当前不可用"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("已向意图 %d 的 work 发送终止信号", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("停做 X、聚焦 Y"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "给一条正在运行的意图(work)实时注入纠偏指令，不打断它、不丢已有进展：worker 会在下一步动作前收到你的指令并据此调整。用于'别再走 X、聚焦 Y'这类【意图内】纠偏；若方向整个错了应改用 kill_work 再下新意图。建议先用 get_worker_output 看它在干嘛。",
		obj(map[string]any{
			"intent_id": idp("要纠偏的意图 id（= work 句柄）"),
			"message":   str("给 worker 的纠偏指令，明确让它停止什么、转向什么"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work 当前不可用"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message 必填"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("已向意图 %d 的 work 注入纠偏指令（下一步生效）", id)), nil
		})
}

// getWorkerOutput returns a work's final (or截至中止时的) conclusion text by intent id.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "取本任务或直接关联任务某条意图(work)的最终输出结论。关联任务结果带 source_task_id/inherited=true 且只读。正常结束返回其总结；被终止(stopped)/异常的 work 返回其截至中止时的最后输出。",
		obj(map[string]any{"intent_id": idp("意图 id（= work 句柄）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id 不属于本任务或其直接关联任务"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "（该 work 尚无任何输出）",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("（该 work 尚无任何输出）"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}
