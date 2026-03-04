#!/usr/bin/env python3
"""
Синхронизация списков моделей CC GUI из models-config.json.

Читает models-config.json (статический конфиг), определяет включённые провайдеры
из .env и записывает модели в JCEF localStorage, не делая HTTP-запросов.

Разделение по CLI:
  Claude Code GUI  — модели с /v1/messages
  Codex GUI        — модели с /v1/responses
"""

from __future__ import annotations

import argparse
import glob as glob_mod
import json
import os
import re
import subprocess
import sys
from pathlib import Path
from typing import Any, Optional

try:
    import plyvel
except ImportError:
    print("ERROR: plyvel is required. Install with: pip install plyvel")
    sys.exit(1)


# ── Paths ────────────────────────────────────────────────────────────────────────

PROJECT_DIR = Path(__file__).resolve().parent.parent   # copilot2api-fork/
CONFIG_PATH = PROJECT_DIR / "models-config.json"
ENV_PATH    = PROJECT_DIR / ".env"
PATCH_SCRIPT = PROJECT_DIR / "patch_cc_gui" / "scripts" / "patch_installed_plugin_auto.sh"

CLAUDE_SETTINGS_PATH = os.path.expanduser("~/.claude/settings.json")
JCEF_BASE = os.path.expanduser("~/.cache/JetBrains")

# ── localStorage keys ────────────────────────────────────────────────────────────

KEY_CLAUDE_CUSTOM        = "claude-custom-models"
KEY_CODEX_CUSTOM         = "codex-custom-models"
KEY_HERMES_CUSTOM        = "hermes-custom-models"
KEY_MODEL_CAPABILITIES   = "ccgui-model-capabilities-v1"
KEY_MODEL_SELECTION_STATE = "model-selection-state"

# LevelDB encoding for Chromium localStorage
LEVELDB_KEY_PREFIX       = b"_file://\x00\x01"
LEVELDB_VALUE_ASCII_PREFIX = b"\x01"

ENV_VARS_TO_REMOVE = [
    "ANTHROPIC_DEFAULT_SONNET_MODEL",
    "ANTHROPIC_SMALL_FAST_MODEL",
    "ANTHROPIC_DEFAULT_OPUS_MODEL",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL",
]


# ── Config loading ───────────────────────────────────────────────────────────────


def load_config() -> dict:
    if not CONFIG_PATH.exists():
        print(f"ERROR: {CONFIG_PATH} not found. Run from the project root or check PLAN.md.")
        sys.exit(1)
    with open(CONFIG_PATH, encoding="utf-8") as f:
        return json.load(f)


def read_env_file(env_path: Path) -> dict[str, str]:
    """Parse a .env file and return key→value pairs (comments and blanks ignored)."""
    result: dict[str, str] = {}
    if not env_path.exists():
        return result
    for line in env_path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if "=" in line:
            key, _, value = line.partition("=")
            result[key.strip()] = value.strip()
    return result


def read_enabled_providers() -> dict[str, bool]:
    """
    Determine which providers are active.
    Values are read from .env; environment variables take precedence.
    """
    env = read_env_file(ENV_PATH)

    def get(key: str, default: str = "") -> str:
        return os.environ.get(key, env.get(key, default))

    copilot_on = get("COPILOT_ON", "true").lower() not in ("false", "0", "no")
    anthropic_on = get("ANTHROPIC_ON", "false").lower() in ("true", "1", "yes")
    deepseek_key = get("DEEPSEEK_API_KEY", "")
    deepseek_on = bool(deepseek_key)

    return {
        "copilot":   copilot_on,
        "deepseek":  deepseek_on,
        "anthropic": anthropic_on,
    }


# ── Model building ───────────────────────────────────────────────────────────────


def format_context_window(context_window: Optional[int]) -> Optional[str]:
    if not context_window or context_window <= 0:
        return None
    if context_window >= 1_000_000:
        value = context_window / 1_000_000
        return f"{int(value)}M" if value.is_integer() else f"{value:.1f}M"
    if context_window >= 1_000:
        value = context_window / 1_000
        return f"{int(value)}K" if value.is_integer() else f"{value:.1f}K"
    return str(context_window)


def make_label(model: dict, provider: str) -> str:
    """Return display_name from config, falling back to a derived label."""
    if model.get("display_name"):
        return model["display_name"]
    return model["id"]


def make_source_label(provider: str) -> str:
    labels = {
        "copilot":   "GitHub Copilot",
        "deepseek":  "DeepSeek API",
        "anthropic": "Anthropic API",
    }
    return labels.get(provider, provider.capitalize())


def describe_capabilities(model: dict) -> str:
    parts: list[str] = []
    cw = format_context_window(model.get("context_window"))
    if cw:
        parts.append(f"ctx: {cw}")
    reasoning = model.get("reasoning_levels", [])
    if reasoning:
        parts.append("reasoning: " + "/".join(reasoning))
    parts.append("1M: да" if model.get("supports_1m") else "1M: нет")
    return " · ".join(parts)


def model_gui_id(model: dict, provider: str) -> str:
    """
    Compute the ID written to CC GUI leveldb.
    DeepSeek with 1M support gets a [1000k] suffix so the CC GUI
    long-context toggle works correctly.
    """
    mid = model["id"]
    if provider == "deepseek" and model.get("supports_1m"):
        return f"{mid}[1000k]"
    return mid


def make_custom_model(model: dict, provider: str) -> dict:
    gui_id = model_gui_id(model, provider)
    label = make_label(model, provider)
    source = make_source_label(provider)
    cap_summary = describe_capabilities(model)
    description_parts = [p for p in (source, cap_summary) if p]
    return {
        "id":          gui_id,
        "label":       label,
        "description": " · ".join(description_parts),
    }


def build_claude_gui_models(config: dict, enabled: dict[str, bool]) -> list[dict]:
    """Models shown in Claude Code GUI: those with /v1/messages endpoint."""
    result = []
    for provider, pdata in config.get("providers", {}).items():
        if not enabled.get(provider):
            continue
        for model in pdata.get("models", []):
            if "/v1/messages" in model.get("endpoints", []):
                result.append(make_custom_model(model, provider))
    return result


def build_codex_gui_models(config: dict, enabled: dict[str, bool]) -> list[dict]:
    """Models shown in Codex GUI: those with /v1/responses endpoint."""
    result = []
    for provider, pdata in config.get("providers", {}).items():
        if not enabled.get(provider):
            continue
        for model in pdata.get("models", []):
            if "/v1/responses" in model.get("endpoints", []):
                result.append(make_custom_model(model, provider))
    return result


def build_hermes_gui_models(config: dict, enabled: dict[str, bool]) -> list[dict]:
    """Models shown in CCH GUI for Hermes Agent: deepseek models via ACP."""
    result = []
    for provider, pdata in config.get("providers", {}).items():
        if not enabled.get(provider):
            continue
        for model in pdata.get("models", []):
            if "/v1/messages" in model.get("endpoints", []):
                result.append(make_custom_model(model, provider))
    return result


def build_capabilities(config: dict, enabled: dict[str, bool]) -> dict:
    """Build the ccgui-model-capabilities-v1 payload.

    The payload is split into buckets that match the patch's ModelCapabilitiesPayload:
      claude_messages — models with /v1/messages endpoint (Claude Code GUI)
      codex_chat      — models with /v1/responses endpoint (Codex GUI)
    """
    claude_messages: dict[str, dict] = {}
    codex_chat: dict[str, dict] = {}

    for provider, pdata in config.get("providers", {}).items():
        if not enabled.get(provider):
            continue
        for model in pdata.get("models", []):
            endpoints = model.get("endpoints", [])
            entry = {
                "context_window":       model.get("context_window"),
                "context_window_label": format_context_window(model.get("context_window")),
                "supports_1m":          bool(model.get("supports_1m")),
                "reasoning_levels":     model.get("reasoning_levels", []),
                "provider":             provider,
                "endpoints":            endpoints,
            }
            gui_id = model_gui_id(model, provider)
            if "/v1/messages" in endpoints:
                claude_messages[gui_id] = entry
            if "/v1/responses" in endpoints:
                codex_chat[gui_id] = entry

    return {
        "claude_messages": claude_messages,
        "codex_chat":      codex_chat,
    }


# ── Plugin patching ──────────────────────────────────────────────────────────────


def patch_cc_gui_plugin(dry_run: bool = False, verbose: bool = False) -> None:
    if not PATCH_SCRIPT.exists():
        print(f"  SKIP patch: {PATCH_SCRIPT} not found")
        return
    if dry_run:
        print(f"  [DRY-RUN] Would run: {PATCH_SCRIPT}")
        return
    result = subprocess.run(
        ["bash", str(PATCH_SCRIPT)],
        capture_output=not verbose,
        text=True,
    )
    if result.returncode == 0:
        print("  CC GUI plugin patched successfully")
    else:
        print(f"  WARNING: patch script exited {result.returncode}")
        if result.stderr:
            print(f"  {result.stderr.strip()}")


# ── LevelDB helpers ──────────────────────────────────────────────────────────────


def find_leveldb_paths(base_dir: str) -> list[Path]:
    pattern = os.path.join(base_dir, "*", "jcef_cache", "Default", "Local Storage", "leveldb")
    return sorted(Path(p) for p in glob_mod.glob(pattern))


def check_leveldb_lock(db_path: Path) -> bool:
    try:
        db = plyvel.DB(str(db_path), create_if_missing=False)
        db.close()
        return False
    except plyvel.IOError:
        # Stale cef_serve subprocess from JCEF may hold the lock
        # even after IDE is closed. Try to kill it and retry.
        _kill_lock_holder(db_path)
        try:
            db = plyvel.DB(str(db_path), create_if_missing=False)
            db.close()
            return False
        except plyvel.IOError:
            return True


def _kill_lock_holder(db_path: Path) -> None:
    """Kill any process holding the LevelDB LOCK file (stale cef_serve)."""
    import subprocess
    lock_file = db_path / "LOCK"
    if not lock_file.exists():
        return
    try:
        out = subprocess.check_output(
            ["fuser", str(lock_file)], stderr=subprocess.DEVNULL,
            timeout=3, text=True
        ).strip()
        if out:
            for pid_str in out.split():
                try:
                    pid = int(pid_str)
                    os.kill(pid, 9)
                    print(f"  Killed stale process {pid} holding {lock_file}")
                except (ValueError, ProcessLookupError):
                    pass
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired, FileNotFoundError):
        pass


def encode_local_storage_payload(payload: object) -> bytes:
    return LEVELDB_VALUE_ASCII_PREFIX + json.dumps(payload, ensure_ascii=True).encode("utf-8")


def decode_local_storage_payload(raw: Optional[bytes]) -> Optional[Any]:
    if not raw:
        return None
    payload = raw[1:] if raw.startswith(LEVELDB_VALUE_ASCII_PREFIX) else raw
    try:
        return json.loads(payload.decode("utf-8"))
    except Exception:
        return None


def read_local_storage_json(db_path: Path, key_name: str) -> Optional[Any]:
    key = LEVELDB_KEY_PREFIX + key_name.encode("utf-8")
    db = plyvel.DB(str(db_path), create_if_missing=False)
    try:
        return decode_local_storage_payload(db.get(key))
    finally:
        db.close()


def write_local_storage_json(db_path: Path, key_name: str, payload: object,
                             dry_run: bool = False) -> int:
    key = LEVELDB_KEY_PREFIX + key_name.encode("utf-8")
    if dry_run:
        size = len(payload) if isinstance(payload, list) else 1
        print(f"  [DRY-RUN] Would write {key_name}: {size} item(s) in {db_path}")
        return size

    encoded = encode_local_storage_payload(payload)
    try:
        db = plyvel.DB(str(db_path), create_if_missing=False)
    except plyvel.IOError as exc:
        print(f"  SKIP {db_path}: cannot open leveldb — {exc}")
        print("  (IDE may have been launched since the lock check. Close it and re-run.)")
        return 0
    try:
        db.put(key, encoded)
    finally:
        db.close()
    return len(payload) if isinstance(payload, list) else 1


def normalize_model_selection_state(db_path: Path, valid_claude_ids: list[str],
                                   valid_codex_ids: list[str],
                                   valid_hermes_ids: list[str],
                                    dry_run: bool = False, verbose: bool = False) -> None:
    state = read_local_storage_json(db_path, KEY_MODEL_SELECTION_STATE)
    if not isinstance(state, dict):
        if verbose:
            print("    model-selection-state: not found or invalid, skipping")
        return

    changed = False

    selected_claude = state.get("claudeModel")
    if valid_claude_ids and (not isinstance(selected_claude, str) or selected_claude not in valid_claude_ids):
        prev = selected_claude if isinstance(selected_claude, str) else "<missing>"
        state["claudeModel"] = valid_claude_ids[0]
        changed = True
        print(f"    claudeModel: {prev} → {valid_claude_ids[0]}")

    selected_codex = state.get("codexModel")
    if valid_codex_ids and (not isinstance(selected_codex, str) or selected_codex not in valid_codex_ids):
        prev = selected_codex if isinstance(selected_codex, str) else "<missing>"
        state["codexModel"] = valid_codex_ids[0]
        changed = True
        print(f"    codexModel: {prev} → {valid_codex_ids[0]}")

    if state.get("codexPermissionMode") == "plan":
        state["codexPermissionMode"] = "default"
        changed = True
        print("    codexPermissionMode: plan → default")

    if changed:
        write_local_storage_json(db_path, KEY_MODEL_SELECTION_STATE, state, dry_run=dry_run)
    elif verbose:
        print("    model-selection-state: no changes needed")


# ── Settings inspection ──────────────────────────────────────────────────────────


def inspect_claude_settings(settings_path: str, remove_overrides: bool = False,
                             dry_run: bool = False) -> None:
    path = Path(settings_path)
    if not path.exists():
        print(f"  Settings file not found: {settings_path}, skipping")
        return

    with open(path, encoding="utf-8") as f:
        config = json.load(f)

    env = config.get("env", {})
    if not isinstance(env, dict):
        print("  No 'env' section in settings.json")
        return

    found = [(key, env[key]) for key in ENV_VARS_TO_REMOVE if key in env]
    if not found:
        print("  No ANTHROPIC_* model overrides found in settings.json")
        return

    if not remove_overrides:
        print("  Found ANTHROPIC_* model overrides in settings.json (left untouched):")
        for key, value in found:
            print(f"    - {key} = {value}")
        print("  NOTE: Re-run with --remove-anthropic-overrides to delete them.")
        return

    action = "[DRY-RUN] Would remove" if dry_run else "Removed"
    if not dry_run:
        for key, _ in found:
            del env[key]
        with open(path, "w", encoding="utf-8") as f:
            json.dump(config, f, indent=2, ensure_ascii=False)
            f.write("\n")

    print(f"  {action} {len(found)} env var(s):")
    for key, value in found:
        print(f"    - {key} (was: {value})")


# ── Main ─────────────────────────────────────────────────────────────────────────


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Sync CC GUI model lists from models-config.json (no HTTP requests)"
    )
    parser.add_argument("--dry-run", action="store_true",
                        help="Show what would be written without making changes")
    parser.add_argument("--verbose", action="store_true",
                        help="Show detailed output")
    parser.add_argument("--remove-anthropic-overrides", action="store_true",
                        help="Remove ANTHROPIC_* model overrides from ~/.claude/settings.json")
    args = parser.parse_args()

    dry_run = args.dry_run
    verbose = args.verbose

    if dry_run:
        print("=" * 60)
        print("DRY RUN — no changes will be made")
        print("=" * 60)

    # 1. Load config and determine enabled providers
    print(f"\n── Loading config from {CONFIG_PATH} ──")
    config = load_config()
    enabled = read_enabled_providers()

    print(f"  Copilot:   {'ON' if enabled['copilot'] else 'OFF'}")
    print(f"  DeepSeek:  {'ON' if enabled['deepseek'] else 'OFF'}")
    print(f"  Anthropic: {'ON' if enabled['anthropic'] else 'OFF'}")

    # 2. Patch CC GUI plugin
    print("\n── Patching CC GUI plugin ──")
    patch_cc_gui_plugin(dry_run=dry_run, verbose=verbose)

    # 3. Build model lists
    claude_models = build_claude_gui_models(config, enabled)
    codex_models  = build_codex_gui_models(config, enabled)
    hermes_models = build_hermes_gui_models(config, enabled)
    capabilities  = build_capabilities(config, enabled)

    print(f"\n── Model lists ──")
    print(f"  Claude Code GUI: {len(claude_models)} model(s)")
    for m in claude_models:
        print(f"    {m['id']}  —  {m['label']}")

    print(f"  Codex GUI:       {len(codex_models)} model(s)")
    for m in codex_models:
        print(f"    {m['id']}  —  {m['label']}")

    print(f"  Hermes (CCH GUI): {len(hermes_models)} model(s)")
    for m in hermes_models:
        print(f"    {m['id']}  —  {m['label']}")

    valid_claude_ids = [m["id"] for m in claude_models]
    valid_codex_ids  = [m["id"] for m in codex_models]
    valid_hermes_ids = [m["id"] for m in hermes_models]

    # 4. Find and write to leveldb
    db_paths = find_leveldb_paths(JCEF_BASE)

    if not db_paths:
        print(f"\n  WARNING: no JCEF leveldb found under {JCEF_BASE}")
        print("  Is the JetBrains IDE installed and has been launched at least once?")
    else:
        print(f"\n── Writing to {len(db_paths)} leveldb path(s) ──")

    skipped_locked = 0
    for db_path in db_paths:
        if check_leveldb_lock(db_path):
            print(f"\n  SKIP: {db_path} is locked (IDE running). Close the IDE and re-run.")
            skipped_locked += 1
            continue

        print(f"\n  {db_path}")

        write_local_storage_json(db_path, KEY_CLAUDE_CUSTOM, claude_models, dry_run=dry_run)
        print(f"    Wrote {KEY_CLAUDE_CUSTOM}: {len(claude_models)} model(s)")

        write_local_storage_json(db_path, KEY_CODEX_CUSTOM, codex_models, dry_run=dry_run)
        print(f"    Wrote {KEY_CODEX_CUSTOM}: {len(codex_models)} model(s)")

        write_local_storage_json(db_path, KEY_HERMES_CUSTOM, hermes_models, dry_run=dry_run)
        print(f"    Wrote {KEY_HERMES_CUSTOM}: {len(hermes_models)} model(s)")

        write_local_storage_json(db_path, KEY_MODEL_CAPABILITIES, capabilities, dry_run=dry_run)
        print(f"    Wrote {KEY_MODEL_CAPABILITIES}: {len(capabilities)} model(s)")

        normalize_model_selection_state(
            db_path, valid_claude_ids, valid_codex_ids, valid_hermes_ids,
            dry_run=dry_run, verbose=verbose,
        )

    if skipped_locked > 0:
        print(f"\n  WARNING: {skipped_locked} LevelDB(s) skipped (IDE running).")
        print("  Close the IDE and re-run to update those databases.")

    # 5. Inspect ~/.claude/settings.json
    print(f"\n── Checking {CLAUDE_SETTINGS_PATH} ──")
    inspect_claude_settings(
        CLAUDE_SETTINGS_PATH,
        remove_overrides=args.remove_anthropic_overrides,
        dry_run=dry_run,
    )

    print("\nDone.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
