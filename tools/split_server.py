# -*- coding: utf-8 -*-
"""server.go wave-1 splitter: pure function moves per docs/refactor/01-spec/server-split.md."""
import re, io, os, sys

SRC = r"D:\Code\Artex\server\server.go"
OUT = r"D:\Code\Artex\server"

text = io.open(SRC, encoding="utf-8").read()
lines = text.split("\n")

# locate import block end
imp_end = 0
for i, l in enumerate(lines):
    if l.startswith(")"):
        imp_end = i
        break
header_pkg = lines[0]  # package server

# find top-level decl starts: ^func with preceding comment lines
starts = []  # (line_idx, name)
for i, l in enumerate(lines):
    if i <= imp_end:
        continue
    m = re.match(r"^func (?:\([^)]*\) )?([A-Za-z0-9_]+)", l)
    if m:
        # walk back over contiguous comment lines
        s = i
        while s - 1 > imp_end and lines[s - 1].lstrip().startswith("//"):
            s -= 1
        starts.append((s, m.group(1), i))

# assign function names to target files (from spec §3)
MAP = {
  "server_llm.go": """loadLLMConfig saveLLMConfig reapplyActiveProfile webSearchFor nonStreamingResolver maxTokensResolver applyLLM loadProfileConfig effectiveProfileForAgent resolveChatAgent chatUnavailableReason providerForProfile chatAgentForProfile invalidateProfileAgents chatAgentRef getLLM setLLM testLLM truncateReply""".split(),
  "server_tasks.go": """listTasks resolvedTaskStatus setActive control controlIntent restoreRerunIntent rerunIntent rerunBlocked createTask validateTaskProfileIDs updateTaskLLMProfiles parseTarget portOr defaultPort schemeForPort llmHost seed seedFirstIntent getTask taskCoverage taskCoverageGraph taskAssetRefs taskScopeList taskScopeAdd taskScopeDelete frontier""".split(),
  "server_findings.go": """findings normFilter resolveFindingAssets resolveAssetIDs findingStats getFinding findingsExport findingProvenanceInTask findingLineage patchFinding deleteFinding""".split(),
  "server_exploration.go": """intents inheritedIntentResult inheritedGraphSnapshot taskIntentHistoryPage explorationGraph explorationNodes nodeAnchoredAssets csvValues activity parseActivitySession activitySessionStore activityHistory tokenStats tokenDailyStats conversationTokens getLogs getLogsHistory streamLogs streamActivity activityDetail taskActivityDetail""".split(),
  "server_traffic.go": """getTraffic getTrafficHosts deleteTraffic deleteTrafficHosts deleteAllTraffic getTrafficExchange getTrafficBlob""".split(),
  "server_settings.go": """getSettings settingsPayload notifyPublicBaseURL notifyDigestIntervalMin pgDetectPython putSettings testWebSearch""".split(),
  "server_chat.go": """mainSessions newMainSession chat cancelTaskChat finishTaskChat taskChatStatus stopChat fallbackChat""".split(),
  "server_misc.go": """health stats getReport getAudit gc""".split(),
}
name2file = {}
for f, names in MAP.items():
    for n in names:
        name2file[n] = f

# build segments: each segment = [start_idx, next_start_idx)
segments = []
for idx, (s, name, fline) in enumerate(starts):
    e = starts[idx + 1][0] if idx + 1 < len(starts) else len(lines)
    segments.append((name, s, e))

buckets = {f: [] for f in MAP}
keep = []
missing, extra = [], []
for name, s, e in segments:
    tgt = name2file.get(name)
    if tgt:
        buckets[tgt].append("\n".join(lines[s:e]).rstrip() + "\n")
    else:
        keep.append((name, s, e))

# coverage check: every mapped name must have been consumed
consumed = {n for n, _, _ in segments}
for n in name2file:
    if n not in consumed:
        missing.append(n)
for n, _, _ in segments:
    if n not in name2file:
        extra.append(n)
if missing:
    print("MISSING (mapped but not found):", missing); sys.exit(1)

# rebuild server.go: header+imports + pre-func decls (type Server 等) + kept segments
first_start = starts[0][0] if starts else len(lines)
keep_src = []
pre_func = "\n".join(lines[imp_end + 1: first_start]).rstrip()
if pre_func:
    keep_src.append(pre_func + "\n")
for name, s, e in keep:
    keep_src.append("\n".join(lines[s:e]).rstrip() + "\n")
new_server = "\n".join(lines[: imp_end + 1]) + "\n\n" + "\n".join(keep_src)
io.open(os.path.join(OUT, "server.go"), "w", encoding="utf-8", newline="\n").write(new_server)

for f, parts in buckets.items():
    if not parts:
        continue
    body = header_pkg + "\n\nimport (\n" + "\n".join(lines[3:imp_end]) + "\n)\n\n" + "\n".join(parts)
    io.open(os.path.join(OUT, f), "w", encoding="utf-8", newline="\n").write(body)
    print(f, len(parts), "funcs")
print("kept in server.go:", [n for n, _, _ in keep])
