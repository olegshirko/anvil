#!/usr/bin/env python3
"""Extract the gzip-compressed kernel payload from Alpine's EFI-wrapped vmlinuz-virt.

The resulting file must be decompressed with gunzip (which tolerates the trailing
padding inside the PE wrapper better than Python's gzip module).
"""
import sys


def main() -> int:
    if len(sys.argv) != 3:
        print(f"Usage: {sys.argv[0]} <vmlinuz-virt> <output.gz>", file=sys.stderr)
        return 1

    input_path = sys.argv[1]
    output_path = sys.argv[2]

    with open(input_path, "rb") as f:
        data = f.read()

    idx = data.find(b"\x1f\x8b\x08")
    if idx <= 0:
        print("gzip magic not found in input", file=sys.stderr)
        return 1

    with open(output_path, "wb") as out:
        out.write(data[idx:])

    print(f"extracted {len(data) - idx} bytes to {output_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
