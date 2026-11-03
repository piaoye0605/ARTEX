# -*- coding: utf-8 -*-
"""agent/tools.go splitter (P2 unit 4): pure decl moves, spec docs/refactor/01-spec/tools-split.md."""
import re, io, os, sys

SRC = r"D:\Code\Artex\agent\tools.go"
OUT = r"D:\Code\Artex\agent"

text = io.open(SRC, encoding="utf-8").read()
lines = text.split("\n")

pkg_idx = next(i for i, l in enumerate(lines) if l.startswith("package "))
imp_open = next(i for i, l in enumerate(lines) if l.strip() == "import (")
imp_end = next(i for i, l in enumerate(lines) if i > imp_open and l.startswith(")"))

starts = []  # (start_idx, key)
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
  "tools_schema.go": """pid pidList obj str intp idp readTool writeTool readExpTool writeExpTool needExploration jsonResult""".split(),
  "tools_overview.go": """compactIntents graphOverview graphOverviewData inheritedMap block:560 overviewTextBudget relatedOverviewBudgetForSources take recentTerminalIntents relatedTaskOverviews inheritedIntentSummaryState compactNode compactFinding""".split(),
  "tools_read.go": """listFindings factsPageSize listFacts factSummaryMax compactFact nodeDetail""".split(),
  "tools_intents.go": """intentItem addOneIntent addIntent listGoals proveGoal goalMet""".split(),
  "tools_writes.go": """addFinding factItem recordOneFact recordFact hintItem addOneHint goalItem addOneGoal setGoals constraintItem addOneConstraint setConstraints addHint""".split(),
  "tools_control.go": """killWorkTool steerWorkTool getWorkerOutput""".split(),
  "tools_trace.go": """traceSteps getWorkerTrace searchAllWorkerTraces listWorkerTraces""".split(),
}
name2file = {}
for f, names in MAP.items():
    for n in names:
        name2file[n] = f

# ToolSet core: 类型、构造、回调 setter、覆盖开关、工具访问器、生命周期锚点、PlannerTools 装配
KEEP = set("""ToolSet SetNotifyGoal SetNotifyHint SetResumeTask SetNotify SetNotifyFinding EnrichTrigger WriteCounts Total String Writes NewToolSet SetTaskID SetCoverageEnabled CoverageDisabled coverageOnlyTools DropCoverageTools GraphOverviewTool ListFindingsTool GetWorkerTraceTool ListWorkerTracesTool SearchWorkerTracesTool NodeDetailTool AddHintTool SetEnrich SetOwnerNode anchorOwner PlannerTools""".split())

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

# rebuild tools.go: package + imports + kept segments
keep_src = []
for key, s, e in keep:
    keep_src.append("\n".join(lines[s:e]).rstrip() + "\n")
new_main = "\n".join(lines[: imp_end + 1]) + "\n\n" + "\n".join(keep_src)
io.open(os.path.join(OUT, "tools.go"), "w", encoding="utf-8", newline="\n").write(new_main)

imports = "\n".join(lines[imp_open + 1 : imp_end])
for f, parts in buckets.items():
    if not parts:
        continue
    body = "package agent\n\nimport (\n" + imports + "\n)\n\n" + "\n".join(parts)
    io.open(os.path.join(OUT, f), "w", encoding="utf-8", newline="\n").write(body)
    print(f, len(parts), "decls")
print("kept in tools.go:", [k for k, _, _ in keep])
