"""Client for the sensord app: stream or poll phone sensors from Python.

    import sensord

    with sensord.connect() as c:
        print(c.get("light"))                     # latest reading, once
        with c.subscribe("accelerometer", 50) as sub:
            for ev in sub:                        # Event(t=..., v=[x, y, z])
                print(ev.t, ev.v)

Standard library only. Protocol: newline-delimited JSON over TCP on
127.0.0.1:47474 (see DESIGN.md in the sensord repo).
"""

import json
import os
import queue
import shutil
import socket
import subprocess
import threading
import time
from typing import NamedTuple

DEFAULT_ADDR = ("127.0.0.1", 47474)

__all__ = ["connect", "Client", "Subscription", "Event", "SensordError"]


class SensordError(Exception):
    """The server rejected a request, or the connection is gone."""


class Event(NamedTuple):
    t: int  # ns, CLOCK_BOOTTIME (Android's elapsedRealtimeNanos)
    v: list  # raw values, as documented for the Android sensor type


_END = object()  # queued to wake consumers when a stream ends


def connect(addr=None):
    """Connect to sensord. addr defaults to $SENSORD_ADDR or 127.0.0.1:47474.

    If nothing is listening at the default address, the sensord app is started
    (am startservice, available in Termux) and the connection retried for up to
    10 s. Set SENSORD_NO_AUTOSTART=1 to disable that.
    """
    if addr is None:
        env = os.environ.get("SENSORD_ADDR")
        if env:
            host, _, port = env.rpartition(":")
            addr = (host, int(port))
        else:
            addr = DEFAULT_ADDR
    try:
        sock = socket.create_connection(addr)
    except ConnectionRefusedError as e:
        if addr != DEFAULT_ADDR or os.environ.get("SENSORD_NO_AUTOSTART") or not _start_app():
            raise SensordError(f"sensord not reachable at {addr[0]}:{addr[1]}: {e}") from e
        sock = _retry(addr, 10)
    except OSError as e:
        raise SensordError(f"sensord not reachable at {addr[0]}:{addr[1]}: {e}") from e
    return Client(sock)


def _start_app():
    am = shutil.which("am")
    if not am:
        return False
    cmd = [am, "startservice", "-n", "dev.tomo.sensord/.SensorService"]
    return subprocess.run(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


def _retry(addr, seconds):
    deadline = time.monotonic() + seconds
    while True:
        time.sleep(0.1)
        try:
            return socket.create_connection(addr)
        except OSError as e:
            if time.monotonic() >= deadline:
                raise SensordError(
                    f"sensord not reachable at {addr[0]}:{addr[1]} (is the app installed?): {e}"
                ) from e


class Client:
    def __init__(self, sock):
        self._sock = sock
        self._file = sock.makefile("rb")
        self._wlock = threading.Lock()
        self._lock = threading.Lock()
        self._next_id = 0
        self._pending = {}  # id -> Queue(1) for the reply
        self._subs = {}  # id -> Subscription
        self._error = None
        self._reader = threading.Thread(target=self._read, name="sensord-reader", daemon=True)
        self._reader.start()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()

    def close(self):
        """Close the connection. The server stops all its subscriptions."""
        try:
            self._sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        self._sock.close()

    def _read(self):
        try:
            for line in self._file:
                m = json.loads(line)
                op = m.get("op")
                if op is None:
                    with self._lock:
                        sub = self._subs.get(m.get("id"))
                    if sub is not None:
                        sub._push(Event(m["t"], m["v"]))
                    continue
                with self._lock:
                    q = self._pending.pop(m.get("id"), None)
                if q is not None:
                    q.put(m)
            err = SensordError("connection closed")
        except (OSError, ValueError) as e:
            err = SensordError(f"connection lost: {e}")
        with self._lock:
            self._error = err
            pending, self._pending = self._pending, {}
            subs, self._subs = self._subs, {}
        for q in pending.values():
            q.put(None)
        for sub in subs.values():
            sub._end()

    def _call(self, req, sub=None):
        with self._lock:
            if self._error:
                raise self._error
            self._next_id += 1
            req["id"] = self._next_id
            q = queue.Queue(1)
            self._pending[req["id"]] = q
            if sub is not None:
                self._subs[req["id"]] = sub  # before sending: no event can be missed
        data = (json.dumps(req) + "\n").encode()
        with self._wlock:
            try:
                self._sock.sendall(data)
            except OSError as e:
                raise SensordError(f"send failed: {e}") from e
        m = q.get()
        if m is None:
            raise self._error
        if m.get("op") == "err":
            if sub is not None:
                with self._lock:
                    self._subs.pop(req["id"], None)
            raise SensordError(m.get("msg", "error"))
        return m

    def sensors(self):
        """All sensors on the device, as dicts (name, type, max_hz, mode, ...)."""
        return self._call({"op": "list"}).get("sensors", [])

    def status(self):
        """Connections and powered sensors, as a dict."""
        return self._call({"op": "status"})["status"]

    def ping(self):
        self._call({"op": "ping"})

    def get(self, sensor, hz=0):
        """Latest reading of sensor.

        If nobody is reading it, the server powers it on and waits for the first
        reading, then keeps it on for 2 s after the last get, so polling in a
        loop stays fast. hz is the rate while warm; 0 means up to 50 Hz.
        """
        m = self._call({"op": "get", "sensor": sensor, "hz": hz})
        return Event(m["t"], m["v"])

    def subscribe(self, sensor, hz=0, buffer=1024):
        """Stream events of sensor at hz (0 = every event).

        sensor is an exact name from sensors() or a type such as
        "accelerometer", "gyroscope", "light". Iterate the returned
        Subscription; close it (or use it as a context manager) when done.
        If the consumer falls more than `buffer` events behind, newer events
        are dropped and counted in Subscription.dropped.
        """
        sub = Subscription(self, buffer)
        m = self._call({"op": "sub", "sensor": sensor, "hz": hz}, sub)
        sub.id, sub.sensor, sub.hz = m["id"], m.get("sensor"), m.get("hz", 0)
        return sub


class Subscription:
    """Iterable stream of Events. After close, events already buffered are
    still yielded, then iteration ends."""

    def __init__(self, client, buffer):
        self._client = client
        self._q = queue.Queue(buffer)
        self._closed = False
        self.id = None
        self.sensor = None  # resolved sensor name
        self.hz = 0  # granted rate; 0 means every event
        self.dropped = 0

    def _push(self, ev):
        if self._closed:
            return
        try:
            self._q.put_nowait(ev)
        except queue.Full:
            self.dropped += 1

    def _end(self):
        self._closed = True
        while True:
            try:
                self._q.put_nowait(_END)
                return
            except queue.Full:
                try:
                    self._q.get_nowait()  # make room for the end marker
                except queue.Empty:
                    pass

    def __iter__(self):
        return self

    def __next__(self):
        ev = self._q.get()
        if ev is _END:
            self._q.put(_END)  # keep later next() calls from blocking
            raise StopIteration
        return ev

    def next(self, timeout=None):
        """Next event, or None after timeout seconds or once closed."""
        try:
            ev = self._q.get(timeout=timeout)
        except queue.Empty:
            return None
        if ev is _END:
            self._q.put(_END)
            return None
        return ev

    def close(self):
        """Stop the stream; the sensor powers down if nobody else reads it."""
        if self._closed:
            return
        self._closed = True
        try:
            self._client._call({"op": "unsub", "sub": self.id})
        except SensordError:
            pass  # connection gone: the server has already dropped it
        with self._client._lock:
            self._client._subs.pop(self.id, None)
        self._end()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
