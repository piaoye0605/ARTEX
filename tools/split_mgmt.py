# -*- coding: utf-8 -*-
"""server_mgmt.go splitter (P2 unit 2): pure decl moves, spec docs/refactor/01-spec/mgmt-split.md.

Treats top-level func/var/const/type as segments so consts travel to their domain file.
Unnamed grouped blocks (e.g. `const (`) are keyed as "block:<line>".
"""
import re, io, os, sys

SRC = r"D:\Code\Artex\server\server_mgmt.go"
OUT = r"D:\Code\Artex\server"

text = io.open(SRC, encoding="utf-8").read()
lines = text.split("\n")

# locate import block end
imp_end = 0
for i, l in enumerate(lines):
    if l.startswith(")"):
        imp_end = i
        break

# find top-level decl starts: func/var/const/type (+ preceding comment lines)
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
  "server_skills.go": """validSkillName skillFileNode fsListSkills fsSkillUsage fsMissingSkills cleanStrs fsCreateSkill fsUpdateSkillMeta rewriteSkillFrontmatter skillNameFromFrontmatter fsUploadSkill readZipEntry fsDeleteSkill skillPathBlocked skillPathRune maxSkillPathLen skillRelPath walkSkillFiles fsListFiles fsReadFile fsWriteFile fsCreateDir fsDeletePath pgSkillVisibility pgToggleSkillVisibility block:1257""".split(),
  "server_agents.go": """reAgentKey pgListAgents pgCreateAgent pgUpdateAgent pgDeleteAgent agentByKey pgSaveAgentConfig pgGetAgent pgGetAgentVisibility pgSetAgentVisibility""".split(),
  "server_prompts.go": """pgSavePrompt pgResetPrompt pgSaveWrapup pgResetWrapup pgSaveTaskTimeoutWrapup pgResetTaskTimeoutWrapup pgListPromptVersions pgPromptVars pgPreviewPrompt globalPromptVars withGlobalVars validateTemplate renderPrompt templateFields""".split(),
  "server_tools_mcp.go": """pgListTools pgUpdateTool pgResetTool pgListMCP pgSaveMCP pgDeleteMCP pgRefreshMCP pgMCPTools""".split(),
  "server_profiles.go": """pgListProfiles pgSaveProfile pgGetLLMRetryPolicy pgSaveLLMRetryPolicy pgDeleteProfile restoreTasksAfterProfileDelete pgActivateProfile pgLLMPoolStatus pgLLMPoolReset pgListModels""".split(),
}
name2file = {}
for f, names in MAP.items():
    for n in names:
        name2file[n] = f

# segments: [start, next_start)
segments = []
for idx, (s, key) in enumerate(starts):
    e = starts[idx + 1][0] if idx + 1 < len(starts) else len(lines)
    segments.append((key, s, e))

# intentionally kept in server_mgmt.go (任务删除域 + 通用 http 小工具 + 通用可见性)
KEEP = set("""pg pathInt decode taskDeleteDrainTimeout canonicalTaskID beginTaskDelete abortTaskDelete pgDeleteTask writeCommittedTaskDelete waitTaskQuiescent pgResourceVisibility pgToggleVisibility""".split())

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
for k, _, _ in segments:
    if k not in name2file and k not in KEEP:
        extra.append(k)
if missing:
    print("MISSING (mapped but not found):", missing); sys.exit(1)
if extra:
    print("EXTRA (found but unmapped):", extra); sys.exit(1)

# rebuild server_mgmt.go: header+imports + kept segments
keep_src = []
for key, s, e in keep:
    keep_src.append("\n".join(lines[s:e]).rstrip() + "\n")
new_main = "\n".join(lines[: imp_end + 1]) + "\n\n" + "\n".join(keep_src)
io.open(os.path.join(OUT, "server_mgmt.go"), "w", encoding="utf-8", newline="\n").write(new_main)

for f, parts in buckets.items():
    if not parts:
        continue
    body = lines[0] + "\n\nimport (\n" + "\n".join(lines[3:imp_end]) + "\n)\n\n" + "\n".join(parts)
    io.open(os.path.join(OUT, f), "w", encoding="utf-8", newline="\n").write(body)
    print(f, len(parts), "decls")
print("kept in server_mgmt.go:", [k for k, _, _ in keep])
