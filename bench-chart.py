#!/usr/bin/env python3
"""LinkedIn bench chart PNG — end-to-end time to a healthy compose stack.

End-to-end = daemon_ready + compose_up_healthy from bench-harness
results/latest.csv (same numbers as the README table). Horizontal bars,
linear scale: anvil's bar being barely visible next to Colima's is the point.
"""

from PIL import Image, ImageDraw, ImageFont

SS = 4
W, H = 1200 * SS, 720 * SS
BG = (255, 255, 255)
FG = (36, 41, 47)
DIM = (110, 119, 129)
GRID = (228, 233, 238)
ANVIL = (63, 81, 245)
COLD = (140, 149, 159)
RESUME = (196, 202, 208)

F_H = ImageFont.truetype("/System/Library/Fonts/HelveticaNeue.ttc", 27 * SS, index=1)
F_SM = ImageFont.truetype("/System/Library/Fonts/HelveticaNeue.ttc", 17 * SS, index=0)
F_LBL = ImageFont.truetype("/System/Library/Fonts/HelveticaNeue.ttc", 21 * SS, index=0)
F_VAL = ImageFont.truetype("/System/Library/Fonts/SFNSMono.ttf", 19 * SS)

# (backend, cold_ms, resume_ms) — daemon ready + compose up healthy, latest.csv
ROWS = [
    ("anvil", 1405, 1476),
    ("Apple Containers", 3118, 2646),
    ("OrbStack", 5408, 3081),
    ("Docker Desktop", 7193, 7092),
    ("Lima", 10117, 7162),
    ("Colima", 13563, 10667),
]
MAX_MS = 14000

img = Image.new("RGB", (W, H), BG)
d = ImageDraw.Draw(img)

d.text((60 * SS, 30 * SS), "Time to a healthy compose stack — milliseconds, lower is better",
       font=F_H, fill=FG)
d.text((60 * SS, 70 * SS),
       "daemon ready + whole stack (db+cache+api+web) healthy · same machine, same workload",
       font=F_SM, fill=DIM)

LX = 320 * SS          # label column start
RX = W - 140 * SS      # right edge of the plot
TY = 130 * SS
RH = 78 * SS           # per-backend row (two bars each)
BW = 26 * SS           # bar thickness
GAP = 8 * SS
BAR_AREA_W = RX - LX

def x_for(ms):
    return LX + int(BAR_AREA_W * ms / MAX_MS)

# Grid lines every 2 s.
for tick in range(0, MAX_MS + 1, 2000):
    x = x_for(tick)
    d.line([x, TY, x, TY + len(ROWS) * RH + 10 * SS], fill=GRID, width=SS)
    label = f"{tick // 1000}s" if tick else "0"
    tw = d.textlength(label, font=F_SM)
    d.text((x - tw / 2, TY + len(ROWS) * RH + 16 * SS), label, font=F_SM, fill=DIM)

y = TY
for name, cold, resume in ROWS:
    anvil = name == "anvil"
    lbl_font = F_VAL if anvil else F_LBL
    d.text((60 * SS, y + RH / 2 - 12 * SS), name, font=lbl_font,
           fill=ANVIL if anvil else FG)

    for ms, dy, color in ((cold, 0, COLD), (resume, BW + GAP, RESUME)):
        by = y + (RH - 2 * BW - GAP) / 2 + dy
        d.rectangle([LX, by, x_for(ms), by + BW], fill=color)
        txt = f"{ms / 1000:.1f}s"
        if anvil:
            # Anvil's bars are too short for inside labels; draw them in
            # anvil-blue past the bar end, connected visually by proximity.
            d.text((x_for(ms) + 10 * SS, by + BW / 2 - 11 * SS), txt,
                   font=F_VAL, fill=ANVIL)
        else:
            tw = d.textlength(txt, font=F_VAL)
            d.text((x_for(ms) - tw - 10 * SS, by + BW / 2 - 11 * SS), txt,
                   font=F_VAL, fill=DIM)
    y += RH

# Legend.
lx = LX
for label, color in (("cold start", COLD), ("resume / warm restart", RESUME)):
    d.rectangle([lx, H - 60 * SS, lx + 22 * SS, H - 44 * SS], fill=color)
    d.text((lx + 30 * SS, H - 62 * SS), label, font=F_SM, fill=DIM)
    lx += 60 * SS + int(d.textlength(label, font=F_SM)) + 40 * SS

img = img.resize((W // SS, H // SS), Image.LANCZOS)
img.save("bench-chart.png")
print("bench-chart.png saved (4x supersampled)")
