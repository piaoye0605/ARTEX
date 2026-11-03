# -*- coding: utf-8 -*-
"""traffic/traffic.go splitter (P2 unit 3): pure decl moves, spec docs/refactor/01-spec/traffic-split.md."""
import re, io, os, sys

SRC = r"D:\Code\Artex\traffic\traffic.go"
OUT = r"D:\Code\Artex\traffic"

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
  "capture.go": """sink Response RequestError maybePassthrough proxyCausedErr record storedBody spill binaryTypes isBinaryBody binaryTag clipBytes truncateUTF8 nullIfEmpty headerLines requestHeaderLines sanitize""".split(),
  "blobs.go": """blobHashRe blobPath Blob BlobRange""".split(),
  "query.go": """ExchangeMeta Search ftsFilter ftsQuote PageQuery sortColumns statusFilter Page Get assembleRaw HostCount Hosts Count query normalizeSearchHost""".split(),
  "delete.go": """DeleteHost DeleteAll allHostTrees compactIndex deleteWhere hostTrees Deleted stagedTrafficPath hostDeleteStageJournal hostDeleteStageMove hostDeleteStageJournalName stageTrees stageTreesForArchive writeHostDeleteStageJournal restoreTrees reapStage DeleteHostsExact HostDeleteStage Deleted StageDeleteHostsExact StageDeleteHostsExactForArchive stageDeleteHostsExact RecoverHostDeleteStages hostsHaveNoExchanges deleteArchivedHosts pathWithin Rollback rollbackLocked Commit""".split(),
  "reclaim.go": """indexBytes reclaim reclaimChunk gcBlobs legacyBlobRefs""".split(),
  "tools.go": """TrafficSearchDescription Tools SeedToolMetas""".split(),
}
name2file = {}
for f, names in MAP.items():
    for n in names:
        name2file[n] = f

KEEP = set("""indexSchema ftsSchema block:89 Traffic Open initIndex hostOnly ProxyAddr SetUpstreamProxy ValidateProxyURL CACertPath Start Close stopping DB clip""".split())

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

# rebuild traffic.go: package doc + package + imports + kept segments
keep_src = []
for key, s, e in keep:
    keep_src.append("\n".join(lines[s:e]).rstrip() + "\n")
new_main = "\n".join(lines[: imp_end + 1]) + "\n\n" + "\n".join(keep_src)
io.open(os.path.join(OUT, "traffic.go"), "w", encoding="utf-8", newline="\n").write(new_main)

imports = "\n".join(lines[imp_open + 1 : imp_end])
for f, parts in buckets.items():
    if not parts:
        continue
    body = "package traffic\n\nimport (\n" + imports + "\n)\n\n" + "\n".join(parts)
    io.open(os.path.join(OUT, f), "w", encoding="utf-8", newline="\n").write(body)
    print(f, len(parts), "decls")
print("kept in traffic.go:", [k for k, _, _ in keep])
