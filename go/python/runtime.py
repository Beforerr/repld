import os
import sys
import socket
import threading
import traceback
import _thread

# Empty prompts so the REPL doesn't leak ">>> " into captured streams, and a
# no-op displayhook so it doesn't echo expression-statement values (e.g. the
# char count returned by the sentinel's stdout.write). We print results ourselves.
sys.ps1 = ""
sys.ps2 = ""
sys.displayhook = lambda value: None

# Avoid no CRLF translation (Windows).
for _stream in (sys.stdout, sys.stderr):
    try:
        _stream.reconfigure(encoding="utf-8", newline="\n")
    except (AttributeError, ValueError):
        pass

# User code evals in this module's globals (__main__), so a program forwarded as
# `repld python3 file.py` (run here at launch) shares state with later evals.
_NS = globals()

_RUNTIME_FILE = "<string>"  # frames from the exec'd runtime carry this filename
_USER_FILE = "<repld>"
_BUSY = False


def _connect():
    addr = os.environ.get("REPLD_CONTROL_ADDR")
    token = os.environ.get("REPLD_CONTROL_TOKEN")
    if not addr:
        return None
    host, _, port = addr.rpartition(":")
    try:
        s = socket.create_connection((host, int(port)))
        s.sendall((token + "\n").encode())
        return s
    except OSError:
        return None


_CONTROL = _connect()


def _write_control(line):
    if _CONTROL is not None:
        try:
            _CONTROL.sendall((line + "\n").encode())
        except OSError:
            pass


def _hex(s):
    return s.encode("utf-8", "surrogatepass").hex()


# Interrupt: a byte on the control socket raises KeyboardInterrupt on the main
# thread, but only mid-eval (_BUSY) so a late/stray byte can't fire at the idle
# prompt. SIGINT is unusable with piped stdin.
def _interrupt_listener():
    while True:
        try:
            b = _CONTROL.recv(1)
        except OSError:
            break
        if not b:
            break
        if _BUSY:
            _thread.interrupt_main()


if _CONTROL is not None:
    threading.Thread(target=_interrupt_listener, daemon=True).start()


def _render_error(exc):
    short = "ERROR: " + "".join(traceback.format_exception_only(type(exc), exc)).strip()
    user = [f for f in traceback.extract_tb(exc.__traceback__) if f.filename != _RUNTIME_FILE]
    smart = short + "\n"
    if user:
        smart = short + "\nTraceback:\n" + "".join(traceback.format_list(user))
    full = "".join(traceback.format_exception(type(exc), exc, exc.__traceback__)).rstrip()
    return short, smart, full


def _repld_run(hexcode):
    global _BUSY
    code = bytes.fromhex(hexcode).decode("utf-8", "surrogatepass")
    _BUSY = True
    try:
        exec(compile(code, _USER_FILE, "exec"), _NS)
        sys.stdout.flush()
        _write_control("OK")
    except BaseException as exc:  # noqa: BLE001 — KeyboardInterrupt is an interrupt
        sys.stdout.flush()
        short, smart, full = _render_error(exc)
        _write_control("ERR %s %s %s" % (_hex(short), _hex(smart), _hex(full)))
    finally:
        _BUSY = False
