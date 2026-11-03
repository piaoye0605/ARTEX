# -*- coding: utf-8 -*-
"""server/engine.go splitter (P2 unit 7): pure decl moves, spec docs/refactor/01-spec/engine-split.md."""
import re, io, os, sys

SRC = r"D:\Code\Artex\server\engine.go"
OUT = r"D:\Code\Artex\server"

text = io.open(SRC, encoding="utf-8").read()
lines = text.split("\n")

imp_open = next(i for i, l in enumerate(lines) if l.strip() == "import (")
imp_end = next(i for i, l in enumerate(lines) if i > imp_open and l.startswith(")"))

starts = []
decl_re = re.compile(r"^(func|var|const|type)\b")
for i, l in enumerate(lines):
    if i <= imp_end:
        continue
    m = decl_re.match(l)
    if m:
        s = i
        while s - 1 > imp_end and lines[s - 1].lstrip().startswith("//"):
            s -= 1
        if m.group(1) in ("var", "const") and re.match(r"^(var|const)\s*\(", l):
            key = "block:%d" % (i + 1)
        else:
            m2 = re.match(r"^(?:func|var|const|type)\s+(?:\([^)]*\)\s*)?([A-Za-z0-9_]+)", l)
            key = m2.group(1)
        starts.append((s, key))

MAP = {
  "engine_errors.go": """isFKViolation dropReason bumpDrop preview block:71 errWorkControlConflict retryableWorkerModelError""".split(),
  "engine_lifecycle.go": """Pause BeginDelete AbortDelete IsDeleting registerTaskRoutines runTaskRoutine StopTask cancelExec Resume execContextFor""".split(),
  "engine_work.go": """registerWork detachWork ControlWork releaseWorkControl transitionIntentState SteerWork drainSteer KillWork""".split(),
  "engine_steer.go": """steerHooks PreToolUse PostToolUse Stop isThinkingOnlyTurn defaultEmptyTurnNudges emptyTurnNudge""".split(),
  "engine_activity.go": """emitActivity appendActivity""".split(),
  "engine_loop.go": """Run plannerHeartbeatInterval resetPlannerTimer plannerLoop workerLoop runWorkerStep""".split(),
  "engine_intent.go": """runIntent runDetachedIntent taskExecutionPaused sleepCtx claimNext""".split(),
}
name2file = {}
for f, names in MAP.items():
    for n in names:
        name2file[n] = f

# 保留在 engine.go：Engine 类型与构造、运行期状态查询、resolver/就绪装配
KEEP = set("""Engine taskRuntime workExecution nextPlannerRound IsPaused Started LastActivity BeginLLMCall EndLLMCall ActiveLLMCalls touch NewEngine Broadcaster SetReadiness SetAgentResolver SetAuthoritativeAgentResolver snapshotFor Ready ReadyFor""".split())

segments = []
for idx, (s, key) in enumerate(starts):
    e = starts[idx + 1][0] if idx + 1 < len(starts) else len(lines)
    segments.append((key, s, e))

buckets = {f: [] for f in MAP}
keep = []
missing, extra = [], []
for key, s, e in segments:
    tgt = name2file.get(key)
    if tgt:
        buckets[tgt].append("\n".join(lines[s:e]).rstrip() + "\n")
    elif key in KEEP:
        keep.append((key, s, e))
    else:
        extra.append(key)

consumed = {k for k, _, _ in segments}
for n in name2file:
    if n not in consumed:
        missing.append(n)
if missing:
    print("MISSING (mapped but not found):", missing); sys.exit(1)
if extra:
    print("EXTRA (found but unmapped):", extra); sys.exit(1)

# 头部注释（import 块与首个声明之间）需保留，否则会随重建丢失
head = lines[imp_end + 1 : starts[0][0]]
head_src = "\n".join(head).strip("\n")

keep_src = []
for key, s, e in keep:
    keep_src.append("\n".join(lines[s:e]).rstrip() + "\n")
new_main = "\n".join(lines[: imp_end + 1]) + "\n"
if head_src:
    new_main += "\n" + head_src + "\n"
new_main += "\n" + "\n".join(keep_src)
io.open(os.path.join(OUT, "engine.go"), "w", encoding="utf-8", newline="\n").write(new_main)

imports = "\n".join(lines[imp_open + 1 : imp_end])
for f, parts in buckets.items():
    if not parts:
        continue
    body = "package server\n\nimport (\n" + imports + "\n)\n\n" + "\n".join(parts)
    io.open(os.path.join(OUT, f), "w", encoding="utf-8", newline="\n").write(body)
    print(f, len(parts), "decls")
print("kept in engine.go:", [k for k, _, _ in keep])
