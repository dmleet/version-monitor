import sys,urllib.request,re
pairs=[l.split() for l in sys.stdin if l.strip()]
try:
    import yaml
except ImportError:
    yaml=None
for repo,url,chart,cur in pairs:
    try:
        txt=urllib.request.urlopen(url.rstrip('/')+'/index.yaml',timeout=30).read().decode()
    except Exception as e:
        print(chart,cur,'ERR',str(e)[:60]);continue
    # crude: find entries block for chart, versions
    m=re.search(r'\n  '+re.escape(chart)+r':\n(.*?)(?=\n  \S|\Z)',txt,re.S)
    blk=m.group(1) if m else ''
    vs=re.findall(r'\n    version: "?v?([0-9][^"\s]*)',blk)
    stable=[v for v in vs if re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+',v)]
    key=lambda v:tuple(int(x) for x in v.split('.'))
    print(f'{chart:24s} {cur:10s} latest={max(stable,key=key) if stable else "?"}')
