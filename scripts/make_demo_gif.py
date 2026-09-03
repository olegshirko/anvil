#!/usr/bin/env python3
"""Generate boot-demo.gif — a terminal-style animation from real timings.

This mirrors the original local-only boot-demo.py: the session is SYNTHETIC
(typed and paced here, not recorded), but every number comes from a real
measurement — re-measure and paste them into SESSION below:

    anvil stop && sleep 1 && /usr/bin/time -p anvil start
    /usr/bin/time -p docker run --rm alpine echo hello from anvil
    (warm) /usr/bin/time -p docker compose -p demo up -d   # demo compose file

Run: python3 scripts/make_demo_gif.py   (writes boot-demo.gif)
"""
from PIL import Image, ImageDraw, ImageFont

W, H = 1000, 560
FRAME_MS = 60
TYPE_MS = 16          # per character
CMD_PAUSE_MS = 410    # after <enter>, before output
OUT_LINE_MS = 90      # between output lines
OUT_PAUSE_MS = 650    # after a command's output, before next prompt
TAIL_HOLD_MS = 2500

BG = (13, 17, 23)       # GitHub dark
GREEN = (63, 185, 80)   # prompt, checkmarks
WHITE = (230, 237, 243) # command text
GRAY = (139, 148, 158)  # output text
BLUE = (88, 166, 255)   # timings

PROMPT = "❯"
FONT = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", 15)
LINE_H = 22
PAD = 24

# (command, [output lines]) — output lines are (text, color) tuples.
SESSION = [
    (
        "/usr/bin/time -p anvil start",
        [
            ("", GRAY),
            ("real\t0.80", BLUE),
            ("user\t0.03", GRAY),
            ("sys\t0.02", GRAY),
        ],
    ),
    (
        "/usr/bin/time -p docker run --rm alpine echo hello from anvil",
        [
            ("hello from anvil", GRAY),
            ("", GRAY),
            ("real\t0.95", BLUE),
            ("user\t0.00", GRAY),
            ("sys\t0.00", GRAY),
        ],
    ),
    (
        "docker compose -p demo up -d",
        [
            ("[+] Running: 3/3", GRAY),
            (" ✔ Container demo-db-1   Started  0.1s", GREEN),
            (" ✔ Container demo-app-1  Started  0.1s", GREEN),
        ],
    ),
]


def build_timeline():
    """Return display lines with their reveal times (ms)."""
    lines = []  # (kind, text, color, reveal_ms)
    t = 400
    for cmd, out in SESSION:
        lines.append(("prompt", PROMPT, GREEN, t))
        for i in range(len(cmd)):
            lines.append(("cmd", cmd[: i + 1], WHITE, t + i * TYPE_MS))
        t += len(cmd) * TYPE_MS + CMD_PAUSE_MS
        for text, color in out:
            lines.append(("out", text, color, t))
            t += OUT_LINE_MS
        t += OUT_PAUSE_MS
    return lines, t


def render():
    lines, total_ms = build_timeline()
    frames = []
    t = 0
    end_frame_ms = total_ms + TAIL_HOLD_MS
    while t < end_frame_ms:
        img = Image.new("RGB", (W, H), BG)
        d = ImageDraw.Draw(img)
        view = []  # (kind, text, color) revealed so far
        for kind, text, color, at in lines:
            if at > t:
                break
            view.append((kind, text, color))
        # Collapse typing prefixes: keep only the newest cmd of a run.
        collapsed = [
            (k, s, c)
            for i, (k, s, c) in enumerate(view)
            if not (k == "cmd" and i + 1 < len(view) and view[i + 1][0] == "cmd")
        ]
        max_lines = (H - 2 * PAD) // LINE_H
        collapsed = collapsed[-max_lines:]

        y = PAD
        for kind, text, color in collapsed:
            d.text((PAD if kind == "prompt" else PAD + 30, y),
                   text, fill=color, font=FONT)
            y += LINE_H
        # Block cursor after the last visible line while "typing".
        if collapsed:
            kind, text, _ = collapsed[-1]
            x = PAD + 30 if kind != "prompt" else PAD
            w = d.textlength(text, font=FONT)
            d.rectangle([x + w + 2, y - LINE_H + 3, x + w + 11, y - 3], fill=GRAY)

        frames.append(img)
        t += FRAME_MS

    frames[0].save(
        "boot-demo.gif",
        save_all=True,
        append_images=frames[1:],
        duration=FRAME_MS,
        loop=0,
    )
    print(f"boot-demo.gif: {len(frames)} frames, {total_ms/1000:.1f}s session")


if __name__ == "__main__":
    render()
