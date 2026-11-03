# -*- coding: utf-8 -*-
"""db/exploration.go splitter (P2 unit 5): pure decl moves, spec docs/refactor/01-spec/exploration-split.md."""
import re, io, os, sys

SRC = r"D:\Code\Artex\db\exploration.go"
OUT = r"D:\Code\Artex\db"

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
  "exploration_nodes.go": """AddNode AddIntent AddGoal OriginFactID Anchor Link SetNodeState UpdateGoalPayload DeleteGoal nodeCols scanNode scanNodes ListByKind ListByKindPage listByKindPageFiltered countByKindFiltered CountFinishedIntents CountOpenIntents GetNode Nodes NodeFilter NodesPage NodesByIDs AddStandaloneFinding""".split(),
  "exploration_graph.go": """Edge EdgesTouching Edges FactsYielded FindingLineage FindingIntents FindingIntentsTerminal Frontier HasActiveIntent HasOpenGoal ClaimIntent Stats""".split(),
  "exploration_intents.go": """ResetRunningIntents ReopenIntent ReopenBlockedIntents SetIntentState CompareAndSetIntentState IntentCleanup SoftDeleteIntent CancelIntent""".split(),
  "exploration_tokens.go": """tokenUsageBucket intentTokenRollup add TokenDailyAll TokenTotal TokenTotalsAll TokenStatsByWorker TokenStatsBySession""".split(),
  "exploration_activity.go": """AppendActivity ActivityList ActivityListForTerminalIntent ActivitySessionFilter cond ActivityPage ActivityPageForTerminalIntent ActivityMaxID MainSession CurrentMainSeg ListMainSessions NewMainSession ActivityDetail""".split(),
  "exploration_trace.go": """scanTrace traceCols ActivityTrace ActivityTraceForTerminalIntent ActivityTraceSearch ActivityTraceSearchForTerminalIntent ActivityTraceSearchTerminalIntents ActivityTraceSearchExcluding AssetRef AssetRefs ActivityByIDs ActivityByIDsForTerminalIntents""".split(),
  "exploration_metrics.go": """GoalCounts FindingSeverityCounts TaskListMetrics TaskListMetricsAll GoalCountsAll LastActivityAll ExplorationDiag""".split(),
}
name2file = {}
for f, names in MAP.items():
    for n in names:
        name2file[n] = f

# 保留在 exploration.go：节点图类型基座 + ExplorationStore 入口 + payload 净化助手
KEEP = set("""ErrIntentStateConflict utf8Clean jsonbClean Node Activity TokenUsage SessionTokenUsage DailyTokenBucket ExplorationStore CreateExploration Exploration ID Root""".split())

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

keep_src = []
for key, s, e in keep:
    keep_src.append("\n".join(lines[s:e]).rstrip() + "\n")
new_main = "\n".join(lines[: imp_end + 1]) + "\n\n" + "\n".join(keep_src)
io.open(os.path.join(OUT, "exploration.go"), "w", encoding="utf-8", newline="\n").write(new_main)

imports = "\n".join(lines[imp_open + 1 : imp_end])
for f, parts in buckets.items():
    if not parts:
        continue
    body = "package db\n\nimport (\n" + imports + "\n)\n\n" + "\n".join(parts)
    io.open(os.path.join(OUT, f), "w", encoding="utf-8", newline="\n").write(body)
    print(f, len(parts), "decls")
print("kept in exploration.go:", [k for k, _, _ in keep])
