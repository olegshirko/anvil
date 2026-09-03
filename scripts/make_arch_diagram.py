#!/usr/bin/env python3
"""Render arch-diagram.png — the two-process architecture picture.

The original was drawn by a local-only script (lost); this is a rewrite
with the same look (GitHub-dark boxes, layered host/guest zones). Rerun
after architectural changes:

    python3 scripts/make_arch_diagram.py
"""
from PIL import Image, ImageDraw, ImageFont

W, H = 1700, 1080
BG = (13, 17, 23)
BOX = (22, 27, 34)
BOX_INNER = (28, 33, 40)
BORDER = (48, 54, 61)
FG = (230, 237, 243)
GRAY = (139, 148, 158)
GREEN = (63, 185, 80)
BLUE = (88, 166, 255)
PURPLE = (188, 140, 255)
ORANGE = (255, 166, 87)

F = lambda s: ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", s)


def text_w(d, s, font):
    return d.textlength(s, font=font)


def box(d, x0, y0, x1, y1, title, color, lines=(), border=None, tfont=None):
    d.rounded_rectangle([x0, y0, x1, y1], radius=10, fill=BOX,
                        outline=border or BORDER, width=2)
    tf = tfont or F(21)
    d.text((x0 + 20, y0 + 16), title, fill=color, font=tf)
    y = y0 + 16 + (tf.size if hasattr(tf, "size") else 28) + 8
    for ln in lines:
        d.text((x0 + 20, y), ln, fill=GRAY, font=F(17))
        y += 26
    return y


def arrow(d, x0, y0, x1, y1, color, label=None, label_side="right"):
    d.line([x0, y0, x1, y1], fill=color, width=2)
    # arrowhead at (x1, y1), vertical only in this diagram
    d.polygon([(x1 - 6, y1 - 10), (x1 + 6, y1 - 10), (x1, y1)], fill=color)
    if label:
        lf = F(16)
        if label_side == "right":
            d.text((x1 + 12, (y0 + y1) // 2 - 10), label, fill=color, font=lf)
        else:
            w = text_w(d, label, lf)
            d.text((x1 - w - 12, (y0 + y1) // 2 - 10), label, fill=color, font=lf)


def main():
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)

    d.text((30, 26), "anvil — two processes, one unix socket",
           fill=FG, font=F(26))

    # --- CLI layer ---
    box(d, 490, 80, 1210, 170, "docker CLI  /  docker compose  /  buildx", BLUE)
    arrow(d, 640, 170, 640, 250, FG, "unix socket  docker.sock", "left")
    d.line([1060, 170, 1060, 250], fill=PURPLE, width=2)
    d.polygon([(1054, 240), (1066, 240), (1060, 250)], fill=PURPLE)
    d.text((1072, 196), "buildkit.sock", fill=PURPLE, font=F(16))

    # --- Host: vz-runner ---
    y = box(d, 300, 250, 1400, 470, "vz-runner  (Swift, host daemon)", GREEN, [
        "• owns the Virtualization.framework VM, snapshot resume (~0.5 s)",
        "• docker.sock proxy → virtio-vsock (plain byte pump)",
        "• buildkit.sock bridge → vsock:1026 (buildx remote driver)",
        "• port forwarder: localhost:<port> → guest container",
        "• auto-restart with backoff, snapshot invalidation",
    ], border=(40, 90, 50))

    # --- vsock channels into the guest ---
    arrow(d, 560, 470, 560, 560, BLUE, "vsock:1025  Docker API", "left")
    arrow(d, 850, 470, 850, 560, GRAY, "vsock:1024  control", "left")
    arrow(d, 1060, 470, 1060, 560, PURPLE, "vsock:1026  buildkit", "right")

    # --- Guest VM ---
    d.rounded_rectangle([250, 560, 1450, 1000], radius=12,
                        fill=BOX_INNER, outline=BORDER, width=2)
    d.text((270, 576), "Linux VM  (arm64, initramfs, no systemd)", fill=FG, font=F(21))

    box(d, 290, 630, 1410, 830, "guest-agent  (Go, PID 1)", GREEN, [
        "• Docker API server: route table → handlers (containers/images/networks/volumes)",
        "• registry auth: X-Registry-Auth → containerd resolver (private pull/push/build)",
        "• port scanner → pushes mappings to vz-runner  •  healthchecks  •  restart monitor",
        "• CNI config generator (per-project bridge networks, cross-container DNS)",
    ], border=(40, 90, 50))

    arrow(d, 850, 830, 850, 880, FG, None)
    box(d, 290, 880, 1410, 975, "containerd  +  CNI plugins  +  buildkitd", ORANGE, [
        "/var/lib on a persistent virtio-blk disk (images, snapshots, build cache)",
    ])

    # port scanner feedback loop (guest → host)
    d.line([1450, 700, 1520, 700, 1520, 360, 1400, 360],
           fill=ORANGE, width=2)
    d.polygon([(1410, 354), (1410, 366), (1400, 360)], fill=ORANGE)
    d.text((1462, 560), "port\nmappings", fill=ORANGE, font=F(15), anchor="mm")

    # virtiofs share
    d.text((270, 1014),
           "virtiofs share /mnt/anvil: guest-agent.log (debug), networks/<name>.json   ·   "
           "containers reach the internet via VZ NAT",
           fill=GRAY, font=F(15))

    img.save("arch-diagram.png")
    print("arch-diagram.png", img.size)


if __name__ == "__main__":
    main()
