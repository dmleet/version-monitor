"""Compare running image tags with the newest same-shaped tag in each registry.

Usage: python -I imgcheck.py <images.tsv>   (lines: namespace<TAB>image)
Read-only: anonymous registry API calls only.
"""
import json
import re
import sys
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor

SKIP_PREFIX = ("registry.k8s.io/kube-", "registry.k8s.io/coredns", "ghcr.io/siderolabs/",
               "dmleet/", "alpine@", "ubuntu:", "nvcr.io/nvidia/cuda:")


def get(url, headers=None, timeout=20):
    req = urllib.request.Request(url, headers=headers or {})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode()), r.headers


def split(image):
    image = image.split("@")[0]
    name, _, tag = image.rpartition(":")
    if "/" not in name.split(":")[0] and not name:
        name, tag = image, "latest"
    first = name.split("/")[0]
    if "." in first or ":" in first:
        reg, repo = first, name[len(first) + 1:]
    else:
        reg, repo = "docker.io", name
    if reg == "docker.io" and "/" not in repo:
        repo = "library/" + repo
    if reg == "lscr.io":
        reg = "ghcr.io"
    return reg, repo, tag


def tags_dockerhub(repo):
    out, url = [], f"https://hub.docker.com/v2/repositories/{repo}/tags?page_size=100&ordering=last_updated"
    for _ in range(5):
        d, _ = get(url)
        out += [t["name"] for t in d.get("results", [])]
        url = d.get("next")
        if not url:
            break
    return out


def tags_v2(reg, repo, token_url=None):
    headers = {}
    if token_url:
        tok, _ = get(token_url)
        headers["Authorization"] = "Bearer " + (tok.get("token") or tok.get("access_token"))
    out, url = [], f"https://{reg}/v2/{repo}/tags/list?n=1000"
    for _ in range(20):
        d, h = get(url, headers)
        out += d.get("tags") or []
        link = h.get("Link")
        if not link:
            break
        nxt = re.search(r"<([^>]+)>", link).group(1)
        url = nxt if nxt.startswith("http") else f"https://{reg}{nxt}"
    return out


def tags_quay(repo):
    out = []
    for page in range(1, 6):
        d, _ = get(f"https://quay.io/api/v1/repository/{repo}/tag/?limit=100&page={page}&onlyActiveTags=true")
        out += [t["name"] for t in d.get("tags", [])]
        if not d.get("has_additional"):
            break
    return out


def fetch(reg, repo):
    if reg == "docker.io":
        return tags_dockerhub(repo)
    if reg == "ghcr.io":
        return tags_v2(reg, repo, f"https://ghcr.io/token?scope=repository:{repo}:pull")
    if reg == "quay.io":
        return tags_quay(repo)
    if reg == "nvcr.io":
        return tags_v2(reg, repo, f"https://nvcr.io/proxy_auth?scope=repository:{repo}:pull")
    return tags_v2(reg, repo)


def shape(tag):
    return re.sub(r"\d+", "#", tag)


def key(tag):
    return tuple(int(x) for x in re.findall(r"\d+", tag))


def check(image):
    if image.startswith(SKIP_PREFIX) or re.search(r":(sha-)?[0-9a-f]{7,}$", image):
        return image, "skip", None
    reg, repo, tag = split(image)
    try:
        tags = fetch(reg, repo)
    except Exception as e:  # noqa: BLE001
        return image, "error", str(e)[:80]
    same = [t for t in tags if shape(t) == shape(tag)]
    if not same:
        return image, "noshape", None
    newest = max(same, key=key)
    if key(newest) <= key(tag):
        return image, "current", newest
    return image, "behind", newest


def major_minor_gap(cur, new):
    a, b = key(cur), key(new)
    if not a or not b:
        return ""
    if a[0] != b[0]:
        return "MAJOR"
    if len(a) > 1 and len(b) > 1 and a[1] != b[1]:
        return "minor"
    return "patch"


images = sorted({l.split("\t")[1].strip() for l in open(sys.argv[1]) if "\t" in l})
with ThreadPoolExecutor(12) as ex:
    res = list(ex.map(check, images))
rank = {"MAJOR": 0, "minor": 1, "patch": 2, "": 3}
behind = sorted((r for r in res if r[1] == "behind"),
                key=lambda r: rank[major_minor_gap(split(r[0])[2], r[2])])
print("BEHIND:")
for img, _, new in behind:
    print(f"  {major_minor_gap(split(img)[2], new):6s} {img}  ->  {new}")
print("CURRENT:", ", ".join(split(r[0])[1].split("/")[-1] for r in res if r[1] == "current"))
for st in ("error", "noshape"):
    xs = [f"{r[0]} ({r[2]})" if r[2] else r[0] for r in res if r[1] == st]
    if xs:
        print(st.upper() + ":", "; ".join(xs))
print("SKIPPED:", len([r for r in res if r[1] == "skip"]))
