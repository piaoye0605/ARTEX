# -*- coding: utf-8 -*-
"""db/assets.go splitter (P2 unit 6): pure decl moves, spec docs/refactor/01-spec/assets-split.md."""
import re, io, os, sys

SRC = r"D:\Code\Artex\db\assets.go"
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
  "assets_domain.go": """UpsertRootDomainReq UpsertRootDomain UpsertSubdomainReq UpsertSubdomain UpsertAppReq UpsertApp""".split(),
  "assets_host.go": """ErrAssetIPInvalid ValidateAssetIP UpsertIPReq UpsertIP AppendIPPort AppendIPBoundDomain linkHostAssets""".split(),
  "assets_service.go": """UpsertHTTPServiceReq UpsertHTTPService UpsertOtherServiceReq UpsertOtherService UpsertEndpointReq UpsertEndpoint""".split(),
  "assets_query.go": """QueryByType CountByType QueryByCompany CountByCompany pageClause QueryByTask CountByTask GetByIDs CountsByType CountsByTypeForTask""".split(),
  "assets_delete.go": """DeleteByTaskID DeleteByCompanyID DeleteByHost DeleteByIDs HostsByTask HostsForTaskDeletion rowsQuerier hostsForTaskDeletion""".split(),
  "assets_scope.go": """withCompanyScopeMutation resolveCompanyWithICP calcCSegment normalizeURL parseURL marshalJSONBArray marshalStringArray marshalPortServices nullableInt""".split(),
}
name2file = {}
for f, names in MAP.items():
    for n in names:
        name2file[n] = f

# 保留在 assets.go：资产域类型基座 + AssetStore 入口 + 列清单/扫描助手
KEEP = set("""Asset AuthItem ParamItem PortService AssetStore Assets Companies assetSelectCols scanAssets scanTypeCounts""".split())

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
io.open(os.path.join(OUT, "assets.go"), "w", encoding="utf-8", newline="\n").write(new_main)

imports = "\n".join(lines[imp_open + 1 : imp_end])
for f, parts in buckets.items():
    if not parts:
        continue
    body = "package db\n\nimport (\n" + imports + "\n)\n\n" + "\n".join(parts)
    io.open(os.path.join(OUT, f), "w", encoding="utf-8", newline="\n").write(body)
    print(f, len(parts), "decls")
print("kept in assets.go:", [k for k, _, _ in keep])
