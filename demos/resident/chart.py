# Copyright 2026 The Agent Substrate Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""chart.py GUESTS.json RESULTS.txt... -o report.html [--class gvisor] [--compact] [--theme light]

Standard library only. GUESTS.json maps a label to {"label", "image",
"size_mb", "touch_mib"}; RESULTS are claimbench --run outputs.

Builds the resident-pool results page: stat tiles, a dumbbell per guest
(plain vs pooled claim p50, ordered by image size), and a line chart of the
resume step against held memory. Reads claimbench's per-claim lines.
"""
import argparse, html, json, re, statistics, sys

P = "p90"
OTHER = ("p50", "max")
LINE = re.compile(r"^(\S+)\s+(\S+)\s+(resident|restore)\s+create\s+([\d.]+)\s+resume\s+([\d.]+)\s+first\s+([\d.]+)\s+total\s+([\d.]+) ms\s*(.*)$")

def pct(xs, p):
    if not xs: return 0.0
    s = sorted(xs); return s[min(len(s)-1, round(p/100*(len(s)-1)))]

def load(paths):
    rows = []
    for path in paths:
        for line in open(path):
            m = LINE.match(line.strip())
            if m and not m.group(8).strip():
                rows.append(dict(template=m.group(1), source=m.group(3), create=float(m.group(4)), resume=float(m.group(5)), first=float(m.group(6)), total=float(m.group(7))))
    return rows

def summarize(rows, guests, cls):
    out = []
    for label, g in sorted(guests.items(), key=lambda kv: kv[1]["size_mb"]):
        rec = dict(g)
        for variant in ("plain", "pooled"):
            rs = [r for r in rows if r["template"] in (f"{label}-{cls}-{variant}", f"{label}-{variant}")]
            rec[variant] = {k: {"p50": pct([r[k] for r in rs], 50), "p90": pct([r[k] for r in rs], 90), "max": max([r[k] for r in rs], default=0)} for k in ("create", "resume", "first", "total")}
            rec[variant]["n"] = len(rs)
            rec[variant]["adopted"] = sum(1 for r in rs if r["source"] == "resident")
        out.append(rec)
    return out

def fmt_mb(mb):
    return f"{mb/1000:.1f} GB" if mb >= 1000 else f"{mb} MB"

def page(guests, title, runtime, compact=False, theme="", pace="one claim every 300 ms"):
    W, PAD_L, PAD_R = 920, 250, 40
    ROW = 56
    # ---- dumbbell
    xmax = max(max(g["plain"]["total"][P], g["pooled"]["total"][P]) for g in guests) * 1.25
    xmax = (int(xmax / 20) + 1) * 20
    def x(v): return PAD_L + (W - PAD_L - PAD_R) * v / xmax
    H1 = 40 + ROW * len(guests) + 40
    d = []
    for t in range(0, int(xmax) + 1, 20 if xmax <= 200 else 50):
        d.append(f'<line class="grid" x1="{x(t):.1f}" y1="30" x2="{x(t):.1f}" y2="{H1-40}"/><text class="tick" x="{x(t):.1f}" y="{H1-20}" text-anchor="middle">{t}</text>')
    d.append(f'<text class="axis" x="{(PAD_L+W-PAD_R)/2:.0f}" y="{H1-2}" text-anchor="middle">claim time, CreateActor to first HTTP response, p90 ms</text>')
    rows_html = []
    for i, g in enumerate(guests):
        cy = 40 + ROW * i + ROW / 2
        a, b = g["plain"]["total"][P], g["pooled"]["total"][P]
        saving = a - b; ratio = a / b if b else 0
        d.append(f'<text class="label" x="{PAD_L-14}" y="{cy-4}" text-anchor="end">{html.escape(g["label"])}</text>')
        held = f'holds {g["touch_mib"]} MiB' if g["touch_mib"] else "holds no extra memory"
        d.append(f'<text class="sub" x="{PAD_L-14}" y="{cy+12}" text-anchor="end">{fmt_mb(g["size_mb"])} image · {held}</text>')
        d.append(f'<line class="conn" x1="{x(b):.1f}" y1="{cy}" x2="{x(a):.1f}" y2="{cy}"/>')
        for v, cls, name in ((a, "plain", "plain"), (b, "pooled", "pooled")):
            s = g[name]
            tip = f'{name} · p50 {s["total"][P]:.0f} ms · p90 {s["total"]["p90"]:.0f} ms · max {s["total"]["max"]:.0f} ms · resume p50 {s["resume"][P]:.0f} ms · {s["adopted"]}/{s["n"]} adopted'
            d.append(f'<g class="mark" tabindex="0" data-tip="{html.escape(tip)}"><circle class="hit" cx="{x(v):.1f}" cy="{cy}" r="14"/><circle class="ring" cx="{x(v):.1f}" cy="{cy}" r="7"/><circle class="dot {cls}" cx="{x(v):.1f}" cy="{cy}" r="5"/></g>')
        d.append(f'<text class="value" x="{x(b)-12:.1f}" y="{cy+4}" text-anchor="end">{b:.0f}</text>')
        d.append(f'<text class="value" x="{x(a)+12:.1f}" y="{cy+4}">{a:.0f}</text>')
        sign = "−" if saving >= 0 else "+"
        d.append(f'<text class="saving" x="{(x(a)+x(b))/2:.1f}" y="{cy-11}" text-anchor="middle">{sign}{abs(saving):.0f} ms · {ratio:.1f}×</text>')
        rows_html.append(f'<tr><td>{html.escape(g["label"])}</td><td>{fmt_mb(g["size_mb"])}</td><td>{g["touch_mib"]}</td>' + "".join(f'<td>{g[v]["total"][P]:.0f} / {g[v]["total"]["p90"]:.0f} / {g[v]["total"]["max"]:.0f}</td><td>{g[v]["resume"][P]:.0f}</td><td>{g[v]["adopted"]}/{g[v]["n"]}</td>' for v in ("plain", "pooled")) + f'<td>{saving:.0f} ms ({ratio:.1f}×)</td></tr>')
    dumbbell = f'<svg viewBox="0 0 {W} {H1}" width="{W}" height="{H1}" role="img" aria-label="claim time per guest, plain versus pooled">{"".join(d)}</svg>'
    # ---- line chart: resume p50 vs held memory
    H2, PAD_L2, PAD_B = 300, 70, 50
    xs = [g["touch_mib"] for g in guests]
    xmax2 = max(xs) * 1.12 or 1
    ymax = max(max(g["plain"]["resume"][P], g["pooled"]["resume"][P]) for g in guests) * 1.2
    ymax = (int(ymax / 20) + 1) * 20
    def x2(v): return PAD_L2 + (W - PAD_L2 - PAD_R) * v / xmax2
    def y2(v): return 20 + (H2 - PAD_B - 20) * (1 - v / ymax)
    l = []
    for t in range(0, int(ymax) + 1, 20 if ymax <= 160 else 50):
        l.append(f'<line class="grid" x1="{PAD_L2}" y1="{y2(t):.1f}" x2="{W-PAD_R}" y2="{y2(t):.1f}"/><text class="tick" x="{PAD_L2-8}" y="{y2(t)+4:.1f}" text-anchor="end">{t}</text>')
    for g in guests:
        l.append(f'<text class="tick" x="{x2(g["touch_mib"]):.1f}" y="{H2-PAD_B+18}" text-anchor="middle">{g["touch_mib"]}</text>')
        l.append(f'<text class="sub" x="{x2(g["touch_mib"]):.1f}" y="{H2-PAD_B+32}" text-anchor="middle">{html.escape(g["label"])}</text>')
    l.append(f'<text class="axis" x="{(PAD_L2+W-PAD_R)/2:.0f}" y="{H2-4}" text-anchor="middle">memory the guest holds, MiB</text>')
    l.append(f'<text class="axis" transform="translate(14,{(H2-PAD_B)/2:.0f}) rotate(-90)" text-anchor="middle">ResumeActor, p90 ms</text>')
    series = []
    for name, key in (("plain", "resume"), ("pooled", "resume")):
        pts = [(x2(g["touch_mib"]), y2(g[name][key][P]), g) for g in guests]
        path = "M" + " L".join(f"{px:.1f},{py:.1f}" for px, py, _ in pts)
        l.append(f'<path class="line {name}" d="{path}"/>')
        for px, py, g in pts:
            l.append(f'<circle class="ring" cx="{px:.1f}" cy="{py:.1f}" r="6"/><circle class="dot {name}" cx="{px:.1f}" cy="{py:.1f}" r="4"/>')
        px, py, g = pts[-1]
        l.append(f'<text class="label" x="{px+10:.1f}" y="{py+4:.1f}">{name} {g[name][key][P]:.0f} ms</text>')
        series.append((name, pts))
    # crosshair hit zones per x
    for i, g in enumerate(guests):
        tip = f'{g["label"]} · holds {g["touch_mib"]} MiB · plain resume p90 {g["plain"]["resume"][P]:.0f} ms (p50 {g["plain"]["resume"]["p50"]:.0f}) · pooled resume p90 {g["pooled"]["resume"][P]:.0f} ms (p50 {g["pooled"]["resume"]["p50"]:.0f})'
        left = (x2(guests[i-1]["touch_mib"]) + x2(g["touch_mib"])) / 2 if i else PAD_L2
        right = (x2(guests[i+1]["touch_mib"]) + x2(g["touch_mib"])) / 2 if i + 1 < len(guests) else W - PAD_R
        l.append(f'<g class="mark" tabindex="0" data-tip="{html.escape(tip)}"><rect class="hit" x="{left:.1f}" y="10" width="{max(right-left,24):.1f}" height="{H2-PAD_B}"/><line class="hair" x1="{x2(g["touch_mib"]):.1f}" y1="20" x2="{x2(g["touch_mib"]):.1f}" y2="{H2-PAD_B}"/></g>')
    linechart = f'<svg viewBox="0 0 {W} {H2}" width="{W}" height="{H2}" role="img" aria-label="resume step against held memory">{"".join(l)}</svg>'
    # ---- tiles
    savings = [g["plain"]["total"][P] - g["pooled"]["total"][P] for g in guests]
    ratios = [g["plain"]["total"][P] / g["pooled"]["total"][P] for g in guests if g["pooled"]["total"][P]]
    adopted = sum(g["pooled"]["adopted"] for g in guests); n = sum(g["pooled"]["n"] for g in guests)
    tiles = [("p90 claim time saved, median across guests", f"{statistics.median(savings):.0f} ms"),
             ("p90 speedup, median", f"{statistics.median(ratios):.1f}×"),
             ("largest guest, plain → pooled, p90", f"{guests[-1]['plain']['total'][P]:.0f} → {guests[-1]['pooled']['total'][P]:.0f} ms"),
             ("pooled claims that adopted a resident", f"{100*adopted/n:.0f}%")]
    tiles_html = "".join(f'<div class="tile"><div class="v">{v}</div><div class="k">{k}</div></div>' for k, v in tiles)
    legend = '<div class="legend"><span><i class="key plain"></i>plain: every claim restores the golden snapshot</span><span><i class="key pooled"></i>pooled: the claim adopts a resident sandbox</span></div>'
    table = ('<table><thead><tr><th>guest</th><th>image</th><th>holds MiB</th><th>plain total p50/p90/max</th><th>plain resume p90</th><th>plain adopted</th><th>pooled total p50/p90/max</th><th>pooled resume p90</th><th>pooled adopted</th><th>saving</th></tr></thead><tbody>' + "".join(rows_html) + '</tbody></table>')
    return f"""<!doctype html><html lang="en"{f' data-theme="{theme}"' if theme else ""}><head><meta charset="utf-8"><title>{html.escape(title)}</title>
<style>
:root{{color-scheme:light;--surface:#fcfcfb;--text:#0b0b0b;--text2:#52514e;--muted:#8a8984;--grid:#e6e5e1;--plain:#eb6834;--pooled:#2a78d6;--tile:#f3f2ef}}
@media (prefers-color-scheme:dark){{:root:where(:not([data-theme="light"])){{color-scheme:dark;--surface:#1a1a19;--text:#f0efec;--text2:#c3c2b7;--muted:#8d8c86;--grid:#2d2d2b;--plain:#d95926;--pooled:#3987e5;--tile:#232322}}}}
:root[data-theme="dark"]{{color-scheme:dark;--surface:#1a1a19;--text:#f0efec;--text2:#c3c2b7;--muted:#8d8c86;--grid:#2d2d2b;--plain:#d95926;--pooled:#3987e5;--tile:#232322}}
body{{margin:0;background:var(--surface);color:var(--text);font:15px/1.45 -apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}}
main{{max-width:960px;margin:0 auto;padding:28px 20px 48px}}
h1{{font-size:24px;font-weight:600;margin:0 0 4px}} h2{{font-size:17px;font-weight:600;margin:34px 0 4px}}
.sub,.axis,.tick{{fill:var(--text2);font-size:12px}} .sub{{color:var(--text2);font-size:13px}}
.tiles{{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin:18px 0 8px}}
.tile{{background:var(--tile);border-radius:10px;padding:12px 14px}} .tile .v{{font-size:26px;font-weight:600;font-variant-numeric:tabular-nums}} .tile .k{{font-size:12px;color:var(--text2)}}
svg{{max-width:100%;height:auto;display:block}}
.grid{{stroke:var(--grid);stroke-width:1}} .conn{{stroke:var(--muted);stroke-width:2;stroke-linecap:round}} .hair{{stroke:var(--muted);stroke-width:1;opacity:0}}
.label{{fill:var(--text);font-size:14px;font-weight:600}} .value{{fill:var(--text);font-size:13px;font-variant-numeric:tabular-nums}} .saving{{fill:var(--text2);font-size:12px}}
.dot.plain{{fill:var(--plain)}} .dot.pooled{{fill:var(--pooled)}} .ring{{fill:var(--surface)}} .hit{{fill:transparent;cursor:pointer}}
.line{{fill:none;stroke-width:2;stroke-linejoin:round;stroke-linecap:round}} .line.plain{{stroke:var(--plain)}} .line.pooled{{stroke:var(--pooled)}}
.mark:hover .dot,.mark:focus .dot{{filter:brightness(1.15)}} .mark:hover .hair,.mark:focus .hair{{opacity:1}} .mark:focus{{outline:none}}
.legend{{display:flex;gap:22px;font-size:13px;color:var(--text2);margin:6px 0 10px}} .key{{display:inline-block;width:18px;height:3px;border-radius:2px;margin-right:7px;vertical-align:middle}} .key.plain{{background:var(--plain)}} .key.pooled{{background:var(--pooled)}}
#tip{{position:fixed;pointer-events:none;background:var(--text);color:var(--surface);padding:6px 9px;border-radius:6px;font-size:12px;display:none;max-width:360px}}
details{{margin-top:26px}} table{{border-collapse:collapse;font-size:13px;font-variant-numeric:tabular-nums;margin-top:8px}} th,td{{text-align:right;padding:5px 10px;border-bottom:1px solid var(--grid)}} th:first-child,td:first-child{{text-align:left}}
footer{{color:var(--text2);font-size:13px;margin-top:30px}}
</style></head><body><main>
<h1>{html.escape(title)}</h1>
<div class="sub">A fresh actor's claim, CreateActor to first HTTP response through the router, with and without a resident pool, across guest images from {fmt_mb(guests[0]["size_mb"])} to {fmt_mb(guests[-1]["size_mb"])}. {html.escape(runtime)}, in-cluster, 30 claims per bar, {html.escape(pace)}. Marks and labels are p90.</div>
<div class="tiles">{tiles_html}</div>
<h2>Claim time per guest</h2>{legend}{dumbbell}
{"" if compact else f'<h2>The resume step against the memory a guest holds</h2><div class="sub">The restore varies with the guest; the adoption does not, because nothing is restored.</div>{legend}{linechart}<details><summary>Table view</summary>{table}</details>'}
<footer>{"Agent Substrate resident pools. p90 over 30 paced claims per bar; the resume step alone is " + f"{statistics.median([g['plain']['resume']['p90'] for g in guests]):.0f} ms plain against {statistics.median([g['pooled']['resume']['p90'] for g in guests]):.0f} ms pooled at the median guest." if compact else "Agent Substrate resident pools. Values are p90 unless noted; hover or focus a mark for p50, max and the adoption count."}</footer>
</main><div id="tip" role="tooltip"></div>
<script>
const tip=document.getElementById('tip');
for(const m of document.querySelectorAll('.mark')){{const show=e=>{{tip.textContent=m.dataset.tip;tip.style.display='block';const r=m.getBoundingClientRect();tip.style.left=Math.min(window.innerWidth-380,(e.clientX||r.left)+12)+'px';tip.style.top=((e.clientY||r.top)+14)+'px';}};m.addEventListener('pointermove',show);m.addEventListener('focus',show);m.addEventListener('pointerleave',()=>tip.style.display='none');m.addEventListener('blur',()=>tip.style.display='none');}}
</script></body></html>"""

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("guests"); ap.add_argument("results", nargs="+"); ap.add_argument("-o", "--out", required=True)
    ap.add_argument("--title", default="Resident pools: claim latency by guest image")
    ap.add_argument("--class", dest="cls", default="gvisor", help="sandbox class whose templates to chart")
    ap.add_argument("--subtitle", default="", help="replaces the runtime words in the subtitle, e.g. 'microVM on one c3-standard-192-metal node'")
    ap.add_argument("--compact", action="store_true", help="tiles and the dumbbell only, for a message attachment")
    ap.add_argument("--theme", default="", choices=["", "light", "dark"], help="pin the color scheme instead of following the viewer's")
    ap.add_argument("--pace", default="one claim every 300 ms", help="how the claims were paced, for the subtitle")
    a = ap.parse_args()
    guests = summarize(load(a.results), json.load(open(a.guests)), a.cls)
    guests = [g for g in guests if g["plain"]["n"] and g["pooled"]["n"]]
    if not guests:
        sys.exit("no complete guests in the results")
    open(a.out, "w").write(page(guests, a.title, a.subtitle or a.cls, a.compact, a.theme, a.pace))
    print("wrote", a.out, "guests:", [g["label"] for g in guests])

if __name__ == "__main__":
    main()
