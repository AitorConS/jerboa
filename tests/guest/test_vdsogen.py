#!/usr/bin/env python3
"""Check runtime-image boundaries without relying on host ELF headers."""
import pathlib
import re
import struct
import subprocess
import sys
import tempfile


def main():
    generator = pathlib.Path(sys.argv[1]).resolve()
    with tempfile.TemporaryDirectory() as td:
        root = pathlib.Path(td)
        image = bytearray(16384)  # trailing debug sections must not become vvar
        image[:7] = b"\x7fELF\x02\x01\x01"
        struct.pack_into("<Q", image, 32, 64)
        struct.pack_into("<Q", image, 40, 12000)
        struct.pack_into("<HHHHH", image, 54, 56, 1, 64, 4, 3)
        struct.pack_into("<IIQQQQQQ", image, 64, 1, 5, 0, 0, 0, 3000, 3000, 4096)
        image[1000:1004] = b"TEST"
        source, output = root / "vdso.so", root / "vdso.c"
        source.write_bytes(image)
        subprocess.run([str(generator), str(source), str(output)], check=True)
        text = output.read_text()
        data = bytes(int(x, 16) for x in re.findall(r"0x([0-9A-F]{2}),", text))
        assert len(data) == 4096
        assert data[1000:1004] == b"TEST"
        assert not any(data[3000:])
        assert not any(data[40:48]) and not any(data[58:64])
        for malformed in (b"bad", bytes(64), image[:100]):
            source.write_bytes(malformed)
            result = subprocess.run([str(generator), str(source), str(output)], capture_output=True)
            assert result.returncode != 0
    print("vdsogen runtime boundaries PASS")


if __name__ == "__main__":
    main()
