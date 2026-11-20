# -*- coding: utf-8 -*-
# ansibled_events: write task-level results as JSON lines for ansibled.
#
# ansibled (https://github.com/timothywashburn/ansibled) runs
# ansible-playbook with this callback enabled and parses the file named by
# ANSIBLED_EVENTS_FILE afterwards. See DESIGN.md, "Callback plugin".

from __future__ import absolute_import, division, print_function

__metaclass__ = type

DOCUMENTATION = r"""
name: ansibled_events
type: aggregate
short_description: Write play, task and result events as JSON lines for ansibled
description:
  - Writes one JSON object per line to the file named by the C(ANSIBLED_EVENTS_FILE)
    environment variable. Does nothing when that variable is unset.
  - Only task-level results are recorded (not individual loop items), plus play
    starts, task starts, handler starts and the final stats.
  - Messages are truncated to 2000 characters.
version_added: "2.15"
requirements:
  - Enable it with C(ANSIBLE_CALLBACKS_ENABLED=ansibled_events).
"""

import json
import os

from ansible.plugins.callback import CallbackBase

MSG_LIMIT = 2000


def _parts(result):
    """Return (task, host, result dict) for a task result on any ansible-core.

    ansible-core 2.19+ has public ``task``/``host``/``result`` properties and
    deprecates the underscored ones; older versions only have the latter.
    """
    try:
        if hasattr(result, "task") and hasattr(result, "host"):
            return result.task, result.host, result.result
    except Exception:  # pragma: no cover - defensive
        pass
    return (
        getattr(result, "_task", None),
        getattr(result, "_host", None),
        getattr(result, "_result", None),
    )


def _safe(fn, default=""):
    try:
        v = fn()
    except Exception:
        return default
    return default if v is None else v


def _text(v):
    if v is None:
        return ""
    if isinstance(v, str):
        s = v
    else:
        try:
            s = json.dumps(v, default=str, ensure_ascii=False)
        except Exception:
            s = str(v)
    if len(s) > MSG_LIMIT:
        s = s[:MSG_LIMIT]
    return s


class CallbackModule(CallbackBase):
    """Aggregate callback that records events for ansibled."""

    CALLBACK_VERSION = 2.0
    CALLBACK_TYPE = "aggregate"
    CALLBACK_NAME = "ansibled_events"
    CALLBACK_NEEDS_ENABLED = True

    def __init__(self, *args, **kwargs):
        super(CallbackModule, self).__init__(*args, **kwargs)
        self._fh = None
        self._playbook = ""
        self._play = ""
        path = os.environ.get("ANSIBLED_EVENTS_FILE")
        if path:
            try:
                self._fh = open(path, "w", encoding="utf-8")
            except Exception as e:
                self._display.warning("ansibled_events: cannot open %s: %s" % (path, e))

    def _emit(self, event, **fields):
        if self._fh is None:
            return
        rec = {"event": event, "playbook": self._playbook}
        rec.update(fields)
        try:
            self._fh.write(json.dumps(rec, default=str, ensure_ascii=False) + "\n")
            self._fh.flush()
        except Exception as e:
            self._display.warning("ansibled_events: write failed: %s" % e)

    # --- playbook / play / task -------------------------------------------

    def v2_playbook_on_start(self, playbook):
        name = _safe(lambda: playbook._file_name)
        self._playbook = os.path.basename(str(name)) if name else ""

    def v2_playbook_on_play_start(self, play):
        self._play = str(_safe(lambda: play.get_name())).strip()
        self._emit("play_start", play=self._play)

    def _task_start(self, task, handler):
        fields = {
            "play": self._play,
            "task": str(_safe(lambda: task.get_name())).strip(),
            "path": str(_safe(lambda: task.get_path())),
        }
        if handler:
            fields["handler"] = True
        self._emit("task_start", **fields)

    def v2_playbook_on_task_start(self, task, is_conditional):
        self._task_start(task, False)

    def v2_playbook_on_handler_task_start(self, task):
        self._task_start(task, True)

    # --- results ----------------------------------------------------------

    def _result(self, status, result, ignore_errors=False):
        task, host, res = _parts(result)
        if not isinstance(res, dict):
            try:
                res = dict(res or {})
            except Exception:
                res = {}
        action = ""
        if task is not None:
            action = _safe(lambda: task.resolved_action) or _safe(lambda: task.action)
        msg = res.get("msg")
        if not msg and status == "failed":
            msg = res.get("stderr") or res.get("reason")
        self._emit(
            "result",
            status=status,
            host=str(_safe(lambda: host.get_name())) if host is not None else "",
            play=self._play,
            task=str(_safe(lambda: task.get_name())).strip() if task is not None else "",
            action=str(action),
            ignore_errors=bool(ignore_errors),
            msg=_text(msg),
        )

    def v2_runner_on_ok(self, result):
        _, _, res = _parts(result)
        changed = False
        try:
            changed = bool(res.get("changed", False))
        except Exception:
            pass
        self._result("changed" if changed else "ok", result)

    def v2_runner_on_failed(self, result, ignore_errors=False):
        self._result("failed", result, ignore_errors=ignore_errors)

    def v2_runner_on_unreachable(self, result):
        self._result("unreachable", result)

    def v2_runner_on_skipped(self, result):
        self._result("skipped", result)

    # --- stats ------------------------------------------------------------

    def v2_playbook_on_stats(self, stats):
        hosts = {}
        for h in sorted(_safe(lambda: stats.processed.keys(), [])):
            s = _safe(lambda: stats.summarize(h), {})
            hosts[h] = {
                k: int(s.get(k, 0) or 0)
                for k in ("ok", "changed", "failures", "unreachable", "skipped", "rescued", "ignored")
            }
        self._emit("stats", hosts=hosts)
        if self._fh is not None:
            try:
                self._fh.close()
            except Exception:
                pass
            self._fh = None
