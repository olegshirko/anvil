#!/usr/bin/env python3
"""Record a real terminal session and render it as boot-demo.gif.

The session runs in a pty (so the outputs and timings are genuine), the
transcript is captured with timestamps, then rendered into terminal-style
frames with Pillow. Re-run after notable CLI/UX changes:

    python3 scripts/make_demo_gif.py
"""
import os
import pty
import re
import select
import subprocess
import sys
import termios
import time

from PIL import Image, ImageDraw, ImageFont

W, H = 1000, 560
FPS = 12
FONT_SIZE = 15
LINE_H = 22
PAD = 24
COLS = 108
BG = (30, 30, 40)
FG = (220, 220, 230)
GREEN = (140, 220, 160)
CYAN = (120, 200, 230)
GRAY = (130, 130, 145)
TITLE_BG = (55, 55, 70)

PROMPT = "❯"

ansi_re = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b[=>]")


def run_session(commands):
    """Run each command in a zsh pty; return (lines, timings).

    lines: list of (kind, text); kind in {"prompt","cmd","out"}.
    timings: appearance offset in seconds for each line.
    """
    pid, fd = pty.fork()
    if pid == 0:
        os.environ["PS1"] = "PROMPT_MARKER "
        os.environ["CLICOLOR"] = "0"
        os.environ["NO_COLOR"] = "1"
        os.execvp("zsh", ["zsh", "--no-rcs", "--no-globalrcs", "-i"])

    def read_until_marker(deadline=60.0):
        buf = b""
        end = time.time() + deadline
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.2)
            if not r:
                continue
            try:
                chunk = os.read(fd, 65536)
            except OSError:
                break
            if not chunk:
                break
            buf += chunk
            if b"PROMPT_MARKER" in buf.split(b"\n")[-1] or buf.endswith(b"# "):
                break
        return buf

    # We render the typing ourselves, so turn the terminal echo off.
    attrs = termios.tcgetattr(fd)
    attrs[3] &= ~(termios.ECHO | termios.ECHOCTL)
    termios.tcsetattr(fd, termios.TCSANOW, attrs)
    read_until_marker(5.0)  # initial prompt
    transcript = []  # (kind, text, t_offset)
    t0 = time.time()
    for human, cmd in commands:
        transcript.append(("prompt", PROMPT, time.time() - t0))
        for i in range(len(cmd)):
            transcript.append(("cmd", cmd[: i + 1], time.time() - t0 + i * 0.012))
        typed_until = time.time() - t0 + len(cmd)*0.012 + 0.05
        os.write(fd, (cmd + "\n").encode())
        out = read_until_marker(90.0)
        clean = ansi_re.sub("", out.decode(errors="replace"))
        out_t = max(time.time() - t0, typed_until)
        for line in clean.splitlines():
            line = line.rstrip()
            # zsh prints a lone % when output lacks a trailing newline.
            if not line or line == "%" or line.startswith("PROMPT_MARKER"):
                continue
            if line == cmd:  # zle redraws the input line even with ECHO off
                continue
            transcript.append(("out", line, out_t))
        time.sleep(0.25)
    os.write(fd, b"exit\n")
    time.sleep(0.3)
    try:
        os.close(fd)
    except OSError:
        pass
    os.waitpid(pid, 0)
    return transcript


def wrap(text, width):
    out = []
    while len(text) > width:
        out.append(text[:width])
        text = text[1 + width :]
    out.append(text)
    return out


def render(transcript, out_path):
    # Expand each transcript entry into wrapped display lines.
    entries = []  # (kind, text, t)
    for kind, text, t in transcript:
        for i, ln in enumerate(wrap(text, COLS)):
            entries.append((kind, ln, t if i == 0 else t + 0.01 * i))

    total = entries[-1][2] if entries else 1
    max_lines = (H - PAD * 2 - 28) // LINE_H
    # Keep the tail visible: scroll offset in lines once we overflow.
    frames = []
    step = 1.0 / FPS
    t = 0.0
    font = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", FONT_SIZE)
    title_font = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", 12)

    prev_img = None
    end_hold = int(2.5 * FPS)
    frame_t = 0.0
    while True:
        visible = [(k, s) for (k, s, et) in entries if et <= frame_t]
        scroll = max(0, len(visible) - max_lines)
        visible = visible[scroll:]

        img = Image.new("RGB", (W, H), BG)
        d = ImageDraw.Draw(img)
        # Title bar
        d.rectangle([0, 0, W, 28], fill=TITLE_BG)
        d.ellipse([14, 10, 26, 22], fill=(255, 95, 86))
        d.ellipse([34, 10, 46, 22], fill=(255, 189, 46))
        d.ellipse([54, 10, 66, 22], fill=(39, 201, 63))
        d.text((W // 2 - 60, 7), "anvil — zsh", fill=GRAY, font=title_font)

        y = 28 + 14
        for kind, s in visible:
            if kind == "prompt":
                d.text((PAD, y), s, fill=GREEN, font=font)
            elif kind == "cmd":
                d.text((PAD + 26, y), s, fill=FG, font=font)
            else:
                d.text((PAD + 26, y), s, fill=(200, 200, 210), font=font)
            y += LINE_H

        # Cursor on the last line.
        if visible:
            d.rectangle([PAD + 26 + d.textlength(visible[-1][1], font=font) + 3,
                         y - LINE_H + 4, PAD + 26 + d.textlength(visible[-1][1], font=font) + 12,
                         y - 4], fill=CYAN)

        if prev_img is not None and img.tobytes() == prev_img.tobytes():
            same += 1
        else:
            same = 0
        frames.append(img)
        prev_img = img

        if frame_t >= total and same >= end_hold:
            break
        frame_t += step

    frames[0].save(
        out_path,
        save_all=True,
        append_images=frames[1:],
        duration=int(1000 / FPS),
        loop=0,
        optimize=True,
    )
    print(f"wrote {out_path}: {len(frames)} frames")


def main():
    if "--dry" in sys.argv:
        transcript = [
            ("prompt", PROMPT, 0.0),
            ("cmd", "/usr/bin/time -p anvil start", 0.05),
            ("out", "anvil: daemon ready (context: anvil)", 0.5),
            ("out", "real\t\t0.51", 0.55),
        ]
        render(transcript, "/tmp/demo-dry.gif")
        return

    subprocess.run(["anvil", "stop"], check=False, capture_output=True)
    time.sleep(1.0)
    # Clean state so the demo session is reproducible (name conflicts, stray
    # containers); the demo container is removed again after recording.
    subprocess.run(["anvil", "start"], check=False, capture_output=True)
    for _ in range(30):
        if subprocess.run(["docker", "version"], capture_output=True).returncode == 0:
            break
        time.sleep(0.5)
    subprocess.run(["docker", "rm", "-f", "web"], check=False, capture_output=True)
    subprocess.run(["anvil", "stop"], check=False, capture_output=True)
    time.sleep(1.0)
    commands = [
        ("/usr/bin/time -p anvil start", "/usr/bin/time -p anvil start"),
        ("docker run --rm alpine echo hello from anvil", "docker run --rm alpine echo hello from anvil"),
        ("docker run -d --name web -p 8080:80 nginx:alpine", "docker run -d --name web -p 8080:80 nginx:alpine"),
        ("docker ps", "docker ps"),
    ]
    transcript = run_session(commands)
    subprocess.run(["docker", "rm", "-f", "web"], check=False, capture_output=True)
    for kind, text, t in transcript:
        if kind != "cmd" or text.endswith(("start", "anvil", "ps")) or " " not in text:
            print(f"{t:6.2f}s {kind:6} {text}")
    render(transcript, "boot-demo.gif")


if __name__ == "__main__":
    main()
