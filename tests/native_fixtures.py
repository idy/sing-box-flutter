"""Small native artifact fixtures shared by build and packaging tests."""
from pathlib import Path


def linux_executable(output: Path, arch: str = "amd64") -> Path:
    """Write a minimal ELF64 header with executable mode for the requested CPU."""
    output.parent.mkdir(parents=True, exist_ok=True)
    header = bytearray(64)
    header[:7] = bytes.fromhex("7f454c46020101")
    header[16:18] = (2).to_bytes(2, "little")
    header[18:20] = {"amd64": 62, "arm64": 183}[arch].to_bytes(2, "little")
    header[20:24] = (1).to_bytes(4, "little")
    header[52:54] = (64).to_bytes(2, "little")
    output.write_bytes(header)
    output.chmod(0o755)
    return output
