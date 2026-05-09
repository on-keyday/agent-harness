"""Venv-bootstrap helper. Pure stdlib (no third-party deps).

ensure_venv() creates ``scripts/.venv`` on first call, installs the deps
from ``scripts/requirements.txt`` into it, and re-execs the calling script
inside the venv's Python interpreter. Subsequent calls are nearly free
(just compares ``requirements.txt`` hash to ``.venv/.req-sha256``).

After ``ensure_venv()`` returns, the current process's ``sys.executable``
is the venv interpreter, so callers can safely import third-party deps
(e.g. psutil) below.

Importable from any of the entry scripts in this directory; assumes
``__file__`` resolves under ``scripts/``.
"""

from __future__ import annotations

import hashlib
import os
import subprocess
import sys
import venv
from pathlib import Path

_SCRIPTS = Path(__file__).resolve().parent
_VENV = _SCRIPTS / ".venv"
_REQ = _SCRIPTS / "requirements.txt"
_HASH_FILE = _VENV / ".req-sha256"


def _venv_python() -> Path:
    if os.name == "nt":
        return _VENV / "Scripts" / "python.exe"
    return _VENV / "bin" / "python"


def _create_venv() -> None:
    sys.stderr.write(f"[scripts] creating venv at {_VENV}\n")
    builder = venv.EnvBuilder(
        with_pip=True,
        clear=False,
        symlinks=(os.name != "nt"),
        upgrade_deps=False,
    )
    builder.create(str(_VENV))


def _ensure_deps(py: Path) -> None:
    if not _REQ.exists():
        return
    want = hashlib.sha256(_REQ.read_bytes()).hexdigest()
    if _HASH_FILE.exists() and _HASH_FILE.read_text(encoding="utf-8").strip() == want:
        return
    sys.stderr.write(f"[scripts] installing deps from {_REQ.name}\n")
    subprocess.check_call(
        [
            str(py),
            "-m",
            "pip",
            "install",
            "--quiet",
            "--disable-pip-version-check",
            "-r",
            str(_REQ),
        ]
    )
    _HASH_FILE.write_text(want, encoding="utf-8")


def _same_python(a: Path, b: Path) -> bool:
    try:
        return a.resolve() == b.resolve()
    except OSError:
        return str(a) == str(b)


def ensure_venv() -> None:
    """Ensure scripts/.venv exists with deps installed; re-exec inside it.

    Returns normally only when the current process is already running
    inside the venv (creating it / installing deps as needed first).
    When a re-exec is required, this function does not return on Unix
    (it ``execv``s) and exits with the child's return code on Windows
    (where ``execv`` has surprising parent-exit semantics).
    """
    py = _venv_python()
    if not py.exists():
        _create_venv()
    _ensure_deps(py)

    if _same_python(Path(sys.executable), py):
        return

    script_path = os.path.abspath(sys.argv[0])
    new_argv = [str(py), script_path, *sys.argv[1:]]
    if os.name == "nt":
        # os.execv on Windows uses _spawnv semantics (parent exits but the
        # caller may still observe it as alive briefly). Use subprocess
        # to keep deterministic exit-code propagation.
        result = subprocess.run(new_argv)
        sys.exit(result.returncode)
    os.execv(str(py), new_argv)
