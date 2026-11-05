package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
)

// Coverage for the one-shot migration face of orchestration.go: every
// seed*/reseed*/upgrade* helper is guarded by a settings flag, and on any DB
// that has already booted once those flags are set — so the bodies only run on
// the very first start. Clearing the guards here re-runs them for real.

// oneShotSeedFlags mirrors the settings guards used by seedOrchestrationTools.
var oneShotSeedFlags = []string{
	"tool_schema_refresh_v7_list_facts_paging",
	"goal_met_unbind_default_v1",
	"goals_prompt_constraint_step_v1",
	"mainagent_prompt_goalless_intent_v1",
	"planner_prompt_compact_realistic_v2",
	"worker_prompt_compact_v4",
	"reporter_trigger_evidence_version_v1",
	"reporter_agent_seed_v1",
	"auto_report_finding_v1",
	"planner_report_finding_v1",
	"planner_list_assets_v1",
	"company_scope_rebind_v1",
	"worker_readtools_unbind_v1",
	"worker_readback_rebind_v2",
	"auto_default_bindings_v3",
}

func clearSeedFlags(t *testing.T, m *Manager) {
	t.Helper()
	for _, f := range oneShotSeedFlags {
		if err := m.pg.SetSetting(f, ""); err != nil {
			t.Fatalf("clear flag %s: %v", f, err)
		}
	}
}

func TestSeedMigrationsRunThenRearm(t *testing.T) {
	m, s := newTestStack(t)
	seeds := agent.BuiltinPromptSeeds()

	// The reporter agent is itself a one-shot seed; drop it so the creation path
	// (agent + prompt + trigger behavior + bindings + trigger) runs.
	if err := m.pg.DeleteAgent("reporter"); err != nil {
		t.Fatalf("delete reporter: %v", err)
	}

	// Give the four prompt reseeds a non-default current version, so they take
	// the "append a new default version" path instead of the equality no-op.
	for _, key := range []string{"goals", "mainagent", "planner", "worker"} {
		a, err := m.pg.GetAgentByKey(key)
		if err != nil || a == nil {
			t.Fatalf("agent %s: %v", key, err)
		}
		if _, err := m.pg.ResetPromptToDefault(a.ID, "自定义占位-"+key); err != nil {
			t.Fatalf("seed custom prompt %s: %v", key, err)
		}
	}

	clearSeedFlags(t, m)
	s.seedOrchestrationTools()

	// One-shot semantics: every guard is re-armed, so a second boot is a no-op.
	for _, f := range oneShotSeedFlags {
		if v, _, _ := m.pg.GetSetting(f); v != "true" {
			t.Fatalf("flag %s not re-armed (value=%q)", f, v)
		}
	}

	// Each reseed restored its agent's prompt to the code default.
	for _, key := range []string{"goals", "mainagent", "planner", "worker"} {
		a, err := m.pg.GetAgentByKey(key)
		if err != nil || a == nil {
			t.Fatalf("agent %s after reseed: %v", key, err)
		}
		cur, err := m.pg.CurrentPrompt(a.ID)
		if err != nil {
			t.Fatalf("current prompt %s: %v", key, err)
		}
		if cur != seeds[key] {
			t.Fatalf("%s prompt not reset to default (len=%d want %d)", key, len(cur), len(seeds[key]))
		}
	}

	// The reporter agent came back with its trigger carrying the current message.
	rep, err := m.pg.GetAgentByKey("reporter")
	if err != nil || rep == nil {
		t.Fatalf("reporter not recreated: %v", err)
	}
	triggers, err := m.pg.ListTriggersFor("reporter")
	if err != nil {
		t.Fatalf("list reporter triggers: %v", err)
	}
	var sawTrigger bool
	for _, tr := range triggers {
		if tr.OnToolCall && tr.ToolCallMessage == reporterToolCallMessage {
			sawTrigger = true
		}
	}
	if !sawTrigger {
		t.Fatalf("reporter trigger missing current message: %+v", triggers)
	}

	// A second pass is a pure no-op (all guards armed) — still consistent.
	s.seedOrchestrationTools()
}

func TestUpgradeReporterTriggerMessage(t *testing.T) {
	m, s := newTestStack(t)

	// Ensure the reporter agent + trigger exist (a prior test may have removed it).
	if a, _ := m.pg.GetAgentByKey("reporter"); a == nil {
		if err := m.pg.SetSetting("reporter_agent_seed_v1", ""); err != nil {
			t.Fatalf("clear reporter flag: %v", err)
		}
		s.seedReporterAgent()
	}
	triggers, err := m.pg.ListTriggersFor("reporter")
	if err != nil || len(triggers) == 0 {
		t.Fatalf("reporter triggers: %v (%d)", err, len(triggers))
	}
	target := triggers[0]
	t.Cleanup(func() {
		target.ToolCallMessage = reporterToolCallMessage
		_ = m.pg.UpdateTrigger(target)
	})

	// A trigger still carrying the v1 default is upgraded in place.
	target.ToolCallMessage = reporterToolCallMessageV1
	if err := m.pg.UpdateTrigger(target); err != nil {
		t.Fatalf("downgrade trigger: %v", err)
	}
	if err := m.pg.SetSetting("reporter_trigger_evidence_version_v1", ""); err != nil {
		t.Fatalf("clear upgrade flag: %v", err)
	}
	s.upgradeReporterTriggerMessage()

	triggers, err = m.pg.ListTriggersFor("reporter")
	if err != nil {
		t.Fatalf("reload triggers: %v", err)
	}
	if triggers[0].ToolCallMessage != reporterToolCallMessage {
		t.Fatalf("v1 message not upgraded: %q", triggers[0].ToolCallMessage)
	}

	// A user-edited message is left untouched (only the exact v1 text migrates).
	target = triggers[0]
	target.ToolCallMessage = "用户自定义的触发文案"
	if err := m.pg.UpdateTrigger(target); err != nil {
		t.Fatalf("customize trigger: %v", err)
	}
	if err := m.pg.SetSetting("reporter_trigger_evidence_version_v1", ""); err != nil {
		t.Fatalf("clear upgrade flag: %v", err)
	}
	s.upgradeReporterTriggerMessage()
	triggers, _ = m.pg.ListTriggersFor("reporter")
	if triggers[0].ToolCallMessage != "用户自定义的触发文案" {
		t.Fatalf("user message overwritten: %q", triggers[0].ToolCallMessage)
	}
}

func TestDeriveTaskStatus(t *testing.T) {
	m, s := newTestStack(t)
	stamp := time.Now().UnixNano()
	newTask := func(suffix string) *Task {
		t.Helper()
		task, err := m.CreateTask(fmt.Sprintf("derive %s %d", suffix, stamp), "goal", nil, 0, 0)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		return task
	}

	// A task that has not been started yet.
	if got := s.deriveTaskStatus(newTask("created")); got != "created" {
		t.Fatalf("created status=%q", got)
	}

	// Terminal lifecycle status wins over everything else, even a paused flag.
	done := newTask("done")
	done.updateLifecycle(func(l *taskLifecycleState) { l.Status = "done"; l.Paused = true })
	if got := s.deriveTaskStatus(done); got != "done" {
		t.Fatalf("terminal status=%q", got)
	}

	// Paused via the task's own lifecycle snapshot.
	pausedTask := newTask("paused-field")
	pausedTask.updateLifecycle(func(l *taskLifecycleState) { l.Paused = true })
	if got := s.deriveTaskStatus(pausedTask); got != "paused" {
		t.Fatalf("lifecycle-paused status=%q", got)
	}

	// ... and via the engine's pause set (runtime pause without a lifecycle write).
	enginePaused := newTask("paused-engine")
	s.engine.paused.Store(enginePaused.ID, true)
	t.Cleanup(func() { s.engine.paused.Delete(enginePaused.ID) })
	if got := s.deriveTaskStatus(enginePaused); got != "paused" {
		t.Fatalf("engine-paused status=%q", got)
	}

	// Running needs a resolvable planner/worker pair AND a started engine loop.
	// resolve is engine-global and a zero-value agent.Planner is not safe to run
	// (its todo map is nil), so the fake lives on a throwaway Server whose only
	// field is its own engine — the live one keeps its resolve untouched.
	running := newTask("running")
	probe := &Server{engine: &Engine{resolve: func(*Task) (*agent.Planner, *agent.Worker) {
		return &agent.Planner{}, &agent.Worker{}
	}}}
	probe.engine.started.Store(running.ID, true)
	if got := probe.deriveTaskStatus(running); got != "running" {
		t.Fatalf("running status=%q", got)
	}

	// Resolvable but not started falls back to created.
	notStarted := newTask("ready-not-started")
	if got := s.deriveTaskStatus(notStarted); got != "created" {
		t.Fatalf("ready-not-started status=%q", got)
	}
}
