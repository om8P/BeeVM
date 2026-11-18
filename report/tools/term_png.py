"""Renders captured command output (report/assets/raw-combined/*.txt) as terminal-style PNG
images (report/assets/term/*.png). The text is exactly what the commands printed (API keys were
masked at capture time). Prompt lines carry the capture timestamp (WSL clock, UTC)."""
import os, re, sys
from PIL import Image, ImageDraw, ImageFont

SRC, OUT = sys.argv[1], sys.argv[2]
os.makedirs(OUT, exist_ok=True)
S = 2  # pixel scale for sharp print
mono = ImageFont.truetype("C:/Windows/Fonts/consola.ttf", 15 * S)
bold = ImageFont.truetype("C:/Windows/Fonts/consolab.ttf", 15 * S)
ui = ImageFont.truetype("C:/Windows/Fonts/segoeui.ttf", 13 * S)
BG, BAR, FG, TS, CMD = (30, 30, 30), (50, 50, 51), (212, 212, 212), (128, 128, 128), (78, 201, 176)
PROMPT = re.compile(r"^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) \$ (.*)$")
WRAP = 118

for name in sorted(os.listdir(SRC)):
    if not name.endswith(".txt"):
        continue
    with open(os.path.join(SRC, name), encoding="utf-8", errors="replace") as f:
        raw = f.read().rstrip().split(chr(10))
    lines = []
    for l in raw:
        l = l.replace(chr(9), "    ")
        m = PROMPT.match(l)
        if m:
            cmd = "ommhatre@LOQ69:~/BeeVM$ " + m.group(2)
            first = WRAP - len(m.group(1)) - 1
            lines.append(("p", m.group(1), cmd[:first]))
            cmd = cmd[first:]
            while cmd:
                lines.append(("c", "", "    " + cmd[:WRAP - 4]))
                cmd = cmd[WRAP - 4:]
        else:
            # wrap long output lines (continuation lines are indented) instead of shrinking the image
            while len(l) > WRAP:
                lines.append(("o", "", l[:WRAP]))
                l = "    " + l[WRAP:]
            lines.append(("o", "", l))
    cw = mono.getbbox("M")[2]
    lh = int(19 * S)
    width_ch = max(len(t) + (len(ts) + 1 if ts else 0) for _, ts, t in lines)
    w = max(760 * S, cw * width_ch + 32 * S)
    bar = 30 * S
    h = bar + 16 * S + lh * len(lines) + 14 * S
    img = Image.new("RGB", (w, h), BG)
    d = ImageDraw.Draw(img)
    d.rectangle([0, 0, w, bar], fill=BAR)
    for i, c in enumerate([(255, 95, 87), (254, 188, 46), (40, 200, 64)]):
        x = 14 * S + i * 18 * S
        d.ellipse([x, 10 * S, x + 11 * S, 21 * S], fill=c)
    d.text((72 * S, 6 * S), "WSL2 Ubuntu 24.04 — bash", font=ui, fill=(200, 200, 200))
    y = bar + 10 * S
    for kind, ts, text in lines:
        x = 16 * S
        if kind == "p":
            d.text((x, y), ts, font=mono, fill=TS)
            x += cw * (len(ts) + 1)
            d.text((x, y), text, font=bold, fill=CMD)
        elif kind == "c":
            d.text((x, y), text, font=bold, fill=CMD)
        else:
            d.text((x, y), text, font=mono, fill=FG)
        y += lh
    img.save(os.path.join(OUT, name[:-4] + ".png"))
    print(name, img.size)
