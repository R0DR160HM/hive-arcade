#!/usr/bin/env python3
"""Pack game executables onto the end of a built arcade.

    python3 tools/bundle.py <arcade executable> <output> <games dir>

Every file in <games dir> goes into an uncompressed zip, and the zip is written
after the arcade's own bytes, followed by a 16-byte trailer: the zip's length as
a little-endian uint64, then b"HIVEARC1". lib/bundle.go reads it back from the
running executable. A `<file>.tag` beside a game is packed too, and is what the
arcade shows as that game's build.
"""

import io
import os
import struct
import sys
import zipfile

MAGIC = b"HIVEARC1"


def main() -> int:
    if len(sys.argv) != 4:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    arcade, output, games = sys.argv[1:]

    packed = io.BytesIO()
    names = sorted(os.listdir(games))
    with zipfile.ZipFile(packed, "w", zipfile.ZIP_STORED, allowZip64=True) as z:
        for name in names:
            path = os.path.join(games, name)
            if os.path.isfile(path):
                z.write(path, arcname=name)
                print(f"  packed {name} ({os.path.getsize(path):,} bytes)")
    payload = packed.getvalue()

    with open(arcade, "rb") as f:
        head = f.read()
    with open(output, "wb") as f:
        f.write(head)
        f.write(payload)
        f.write(struct.pack("<Q", len(payload)))
        f.write(MAGIC)
    os.chmod(output, 0o755)
    print(f"{output}: {len(head):,} bytes of arcade + {len(payload):,} bytes of games")
    return 0


if __name__ == "__main__":
    sys.exit(main())
